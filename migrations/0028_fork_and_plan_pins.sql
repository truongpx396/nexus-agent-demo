-- A fork's provenance and a plan run's pinned version move off the session
-- aggregate and into the features that own them (spec 005 T037). sessions carried
-- forked_from_session_id, fork_seq and fork_overrides for runctl.Fork, and plan_id
-- and plan_version for plan.Executor; each was read or written by exactly one
-- feature, and the fork columns by none after the write.
--
--   session_forks  runctl: which session a fork was taken from, at which event, and
--                  what it changed. It is a record; nothing reads it back yet.
--   plan_sessions  plan: the plan version a session is a run of, pinned when the run
--                  starts so a later edit can never change what an in-flight run
--                  executes (task 8.4). The reference to orchestration_plans stays,
--                  because both tables are plan's own.
--
-- Not expand/contract (.claude/rules/migrations.md): see 0026. Nothing has been
-- deployed, so the tables, the backfill and the drop share one transaction.
CREATE TABLE session_forks (
    session_id              uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES tenants (tenant_id),
    forked_from_session_id  uuid NOT NULL,
    fork_seq                bigint NOT NULL,
    overrides               jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at              timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX session_forks_parent_idx ON session_forks (forked_from_session_id);

ALTER TABLE session_forks ENABLE ROW LEVEL SECURITY;
ALTER TABLE session_forks FORCE ROW LEVEL SECURITY;

CREATE POLICY session_forks_isolation ON session_forks
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE plan_sessions (
    session_id    uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL REFERENCES tenants (tenant_id),
    plan_id       uuid NOT NULL,
    plan_version  int  NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),

    FOREIGN KEY (plan_id, plan_version) REFERENCES orchestration_plans (plan_id, version)
);

CREATE INDEX plan_sessions_plan_idx ON plan_sessions (plan_id, plan_version);

ALTER TABLE plan_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE plan_sessions FORCE ROW LEVEL SECURITY;

CREATE POLICY plan_sessions_isolation ON plan_sessions
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- migrate runs as the owner, which bypasses row-level security, so these cover
-- every tenant.
INSERT INTO session_forks (session_id, tenant_id, forked_from_session_id, fork_seq, overrides, created_at)
SELECT session_id, tenant_id, forked_from_session_id, COALESCE(fork_seq, 0), COALESCE(fork_overrides, '{}'::jsonb), created_at
FROM sessions
WHERE forked_from_session_id IS NOT NULL;

INSERT INTO plan_sessions (session_id, tenant_id, plan_id, plan_version, created_at)
SELECT session_id, tenant_id, plan_id, plan_version, created_at
FROM sessions
WHERE plan_id IS NOT NULL AND plan_version IS NOT NULL;

-- Dropping the columns drops the foreign key from forked_from_session_id back to
-- sessions with them.
ALTER TABLE sessions
    DROP COLUMN forked_from_session_id,
    DROP COLUMN fork_seq,
    DROP COLUMN fork_overrides,
    DROP COLUMN plan_id,
    DROP COLUMN plan_version;
