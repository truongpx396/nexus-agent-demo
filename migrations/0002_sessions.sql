-- The only mutable runtime state is the append-only event log (0003); this
-- table (and every column marked PROJECTION below) is a read path derived
-- from that log by replay, never an independent source of truth (FR-086).
--
-- Several columns exist before the phase that populates them (agent_id,
-- plan_id, harness_digest, the delegation-chain columns) on purpose: they
-- are the seams the source design ships in Increment 1 even when the
-- behavior lands later, because retrofitting a column onto historical rows
-- is exactly the migration an append-only log exists to avoid.
CREATE TABLE sessions (
    session_id              uuid PRIMARY KEY,
    session_key             text NOT NULL,
    tenant_id               uuid NOT NULL REFERENCES tenants (tenant_id),

    surface_id              text NOT NULL,
    user_id                 uuid NOT NULL,

    audience_ref            text,

    -- agent_id/agent_version have no FK yet: the agent-config table lands
    -- with Phase 2/3. Pinned at run start regardless (FR-088).
    agent_id                uuid NOT NULL,
    agent_version           int NOT NULL,

    -- Identity of ALL behavior-determining config in force, pinned at run
    -- start (internal/harness.Digest), never changed mid-run (FR-129).
    harness_digest          bytea NOT NULL,

    forked_from_session_id  uuid REFERENCES sessions (session_id),
    fork_seq                bigint,
    fork_overrides          jsonb,

    data_label              text NOT NULL DEFAULT 'internal',
    route_model_id          text NOT NULL DEFAULT '',
    route_reason            jsonb NOT NULL DEFAULT '{}'::jsonb,

    execution_class         text NOT NULL DEFAULT 'interactive',
    priority                int NOT NULL DEFAULT 0,
    region                  text NOT NULL DEFAULT 'local',

    parent_session_id       uuid REFERENCES sessions (session_id),
    root_session_id         uuid NOT NULL,
    depth                   int NOT NULL DEFAULT 0,
    delegation_role         text NOT NULL DEFAULT 'root',

    plan_id                 uuid,
    plan_version            int,

    taint_state             jsonb NOT NULL DEFAULT '{}'::jsonb,      -- PROJECTION
    status                  text NOT NULL DEFAULT 'queued',           -- PROJECTION
    autonomy_level          text NOT NULL DEFAULT 'supervised',
    terminal_reason         text,                                    -- PROJECTION

    active_ms               bigint NOT NULL DEFAULT 0,                -- PROJECTION
    suspended_ms            bigint NOT NULL DEFAULT 0,                -- PROJECTION

    created_at              timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX sessions_tenant_session_key_idx ON sessions (tenant_id, session_key);
CREATE INDEX sessions_root_session_id_idx ON sessions (root_session_id);

-- See 0001_tenants.sql: the real enforcement point is nexus_app
-- (0000_app_role.sql) — nexus, the migration/owner role, is a superuser and
-- bypasses RLS regardless of ENABLE/FORCE.
ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;

CREATE POLICY sessions_isolation ON sessions
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
