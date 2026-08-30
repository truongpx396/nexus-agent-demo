-- Sandbox instances (README.md §5, task 5.12): internal/sandbox tracks one
-- row per Docker container it creates, so a breach (timeout/OOM/PID-limit)
-- or an orderly reclaim is auditable and a crashed nexusd process has
-- something to reconcile against rather than leaking containers silently.
-- isolation carries "gvisor"/"kata" as unshipped values on purpose (task
-- 5.12's own wording) — the column exists so a later phase's stronger
-- isolation backend is a config change, not a schema change.
CREATE TABLE sandboxes (
    sandbox_id      uuid PRIMARY KEY,
    tenant_id       uuid NOT NULL REFERENCES tenants (tenant_id),
    session_id      uuid NOT NULL REFERENCES sessions (session_id),
    container_id    text,
    isolation       text NOT NULL DEFAULT 'docker',  -- "docker" (shipped) | "gvisor" | "kata" (unshipped)
    status          text NOT NULL DEFAULT 'active',  -- active | reclaimed | breached
    breach_reason   text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    reclaimed_at    timestamptz,

    CHECK (status IN ('active', 'reclaimed', 'breached')),
    CHECK (isolation IN ('docker', 'gvisor', 'kata'))
);

CREATE INDEX sandboxes_session_idx ON sandboxes (session_id);
CREATE INDEX sandboxes_active_idx ON sandboxes (tenant_id) WHERE status = 'active';

ALTER TABLE sandboxes ENABLE ROW LEVEL SECURITY;
ALTER TABLE sandboxes FORCE ROW LEVEL SECURITY;

CREATE POLICY sandboxes_isolation ON sandboxes
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
