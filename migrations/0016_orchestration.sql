-- Orchestration plane + delegation (README.md §8, Phase 8): the plan
-- schema/lifecycle (internal/plan) and the delegation transaction
-- (internal/delegate). Both are brand-new tables — no existing table needs
-- an ALTER for this phase: the delegation-chain columns (parent_session_id,
-- root_session_id, depth, delegation_role) and plan_id/plan_version already
-- shipped as seams on `sessions` in migrations/0002_sessions.sql, and
-- checkpoints.open_delegations already shipped in migrations/0013 — this
-- migration is purely additive.

-- delegations: one row per child a `delegate` tool call (or one member of a
-- delegate_fanout plan step's cohort) spawned. parent_tool_use_event_id
-- starts NULL and is bound by kernel.OnDelegate right after
-- suspendForDelegation appends EventDelegationRequested — mirroring how
-- approvals.tool_use_event_id is only known once the gating tool_use event
-- already exists (internal/oversight/approval.go's own Create). status
-- transitions pending -> returned | reaped | bound_exceeded, each terminal
-- and each the trigger for exactly one EventDelegationReturned/Reaped on the
-- PARENT (never the child, which logs its own ordinary terminal event).
CREATE TABLE delegations (
    delegation_id            uuid PRIMARY KEY,
    tenant_id                 uuid NOT NULL REFERENCES tenants (tenant_id),
    parent_session_id          uuid NOT NULL REFERENCES sessions (session_id),
    child_session_id            uuid NOT NULL REFERENCES sessions (session_id),
    parent_tool_use_event_id     uuid,
    fanout_id                     uuid,              -- shared by every child of one delegate_fanout step; NULL for an ad hoc `delegate` call
    agent_id                      text NOT NULL,
    task                          text NOT NULL,
    scope_grant                   jsonb NOT NULL,     -- tool refs granted to the child; a provable subset of the admitted catalog
    return_schema                  jsonb,              -- optional structured-return schema the child's result is validated against
    status                          text NOT NULL DEFAULT 'pending',
    result                          jsonb,
    reason                          text,
    created_at                      timestamptz NOT NULL DEFAULT now(),
    resolved_at                     timestamptz,

    CHECK (status IN ('pending', 'returned', 'reaped', 'bound_exceeded'))
);

CREATE INDEX delegations_parent_idx ON delegations (parent_session_id, status);
CREATE INDEX delegations_child_idx ON delegations (child_session_id);
CREATE INDEX delegations_fanout_idx ON delegations (fanout_id) WHERE fanout_id IS NOT NULL;

ALTER TABLE delegations ENABLE ROW LEVEL SECURITY;
ALTER TABLE delegations FORCE ROW LEVEL SECURITY;

CREATE POLICY delegations_isolation ON delegations
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- fanout_envelopes (README task 8.13): a delegate_fanout plan step reserves
-- ONE ceiling, sized for its worst-case child count, BEFORE the first child
-- starts. remaining_micros is drawn down atomically by each child's own
-- turn-level reservation (internal/delegate.EnvelopeBudgetGate) via a plain
-- conditional UPDATE — Postgres row-level locking gives atomicity for free,
-- and drawing from it never touches internal/cost's tenant-scoped Redis
-- counter at all, which is what makes "per-child reservation against the
-- tenant counter is prohibited" true by construction rather than by
-- convention.
CREATE TABLE fanout_envelopes (
    envelope_id       uuid PRIMARY KEY,
    tenant_id          uuid NOT NULL REFERENCES tenants (tenant_id),
    plan_session_id     uuid NOT NULL REFERENCES sessions (session_id),
    currency             text NOT NULL DEFAULT 'USD',
    ceiling_micros        bigint NOT NULL,
    remaining_micros       bigint NOT NULL,
    child_count              int NOT NULL,
    created_at                timestamptz NOT NULL DEFAULT now(),

    CHECK (remaining_micros >= 0),
    CHECK (remaining_micros <= ceiling_micros)
);

CREATE INDEX fanout_envelopes_plan_session_idx ON fanout_envelopes (plan_session_id);

ALTER TABLE fanout_envelopes ENABLE ROW LEVEL SECURITY;
ALTER TABLE fanout_envelopes FORCE ROW LEVEL SECURITY;

CREATE POLICY fanout_envelopes_isolation ON fanout_envelopes
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- orchestration_plans (README tasks 8.1-8.4): the declarative Plan{steps[],
-- cost_envelope} spec plus its lifecycle. spec is the FULL internal/plan.Plan
-- shape, encoded as closed JSON — a predicate is data (internal/plan's own
-- AST), never a string expression, which is what makes the zero-token
-- routing claim a property of the format rather than a convention. Each
-- (plan_id, version) row is immutable once status reaches 'enabled' (task
-- 8.4: "enabled versions immutable; in-flight runs finish on their
-- version") — enforced in internal/plan/lifecycle.go, not by the schema,
-- the same "the code enforces it, the schema just stores it" division this
-- codebase uses for every other lifecycle (approvals, budgets, ...).
CREATE TABLE orchestration_plans (
    plan_id            uuid NOT NULL,
    tenant_id           uuid NOT NULL REFERENCES tenants (tenant_id),
    version              int NOT NULL,
    name                  text NOT NULL,
    spec                  jsonb NOT NULL,
    status                 text NOT NULL DEFAULT 'draft',
    agent_version           int,
    route_model_id           text,
    created_by                text,
    signed_off_by              text,
    enabled_at                  timestamptz,
    created_at                   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (plan_id, version),
    CHECK (status IN ('draft', 'validated', 'eval_passed', 'signed_off', 'enabled', 'retired'))
);

CREATE INDEX orchestration_plans_tenant_idx ON orchestration_plans (tenant_id, plan_id, version DESC);

ALTER TABLE orchestration_plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE orchestration_plans FORCE ROW LEVEL SECURITY;

CREATE POLICY orchestration_plans_isolation ON orchestration_plans
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
