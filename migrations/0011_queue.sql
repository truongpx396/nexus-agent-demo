-- The durable job queue (README task 6.1): a Postgres table polled with
-- SELECT ... FOR UPDATE SKIP LOCKED rather than a message broker (README §2's
-- infrastructure collapse — NATS JetStream is deferred). One row is one unit
-- of asynchronous, session-scoped control work a worker pool should pick up:
-- resuming a session after a crash, forking one, or draining a steer — never
-- a fresh interactive run, which stays on internal/surfaces/rest's existing
-- synchronous fast path (cmd/nexusd's own wiring comment explains why).
--
-- payload carries only non-sensitive control arguments (ids, an at_seq, a
-- model override string) — never a plaintext user message — so, unlike
-- events.payload, it needs no key_id/envelope: nothing routed through this
-- table is content the trust surface's encryption boundary is meant to
-- cover in the first place.
CREATE TABLE queue_jobs (
    job_id            uuid PRIMARY KEY,
    tenant_id         uuid NOT NULL REFERENCES tenants (tenant_id),
    session_id        uuid NOT NULL REFERENCES sessions (session_id),
    session_key       text NOT NULL,              -- what the Redis session lock (task 6.2) binds to
    kind              text NOT NULL,               -- "resume" | "fork" | "steer"
    payload           jsonb NOT NULL DEFAULT '{}'::jsonb,
    status            text NOT NULL DEFAULT 'pending', -- pending | leased | done | failed
    attempts          int NOT NULL DEFAULT 0,
    available_at      timestamptz NOT NULL DEFAULT now(), -- backoff: not leasable before this
    lease_owner       text,
    lease_expires_at  timestamptz,
    last_error        text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    CHECK (kind IN ('resume', 'fork', 'steer')),
    CHECK (status IN ('pending', 'leased', 'done', 'failed'))
);

-- The SKIP LOCKED lease query (internal/queue/postgres.go) filters on
-- exactly this shape: pending rows whose backoff has elapsed, oldest first.
CREATE INDEX queue_jobs_leasable_idx ON queue_jobs (available_at) WHERE status = 'pending';
CREATE INDEX queue_jobs_session_idx ON queue_jobs (session_id);

-- Enabled and forced like every other tenant table (day-one, co-equal
-- perimeter controls) even though the worker pool's own Lease/Complete/Fail
-- calls connect as the admin/migration role and bypass it on purpose — the
-- same precedent cmd/nexusd's listTenantIDs and runErase's session-to-tenant
-- lookup already set: leasing "the next job for ANY tenant" is a genuinely
-- cross-tenant admin read store.Store.InTenantTx has no way to express. RLS
-- here is defense in depth for nexus_app (which never touches this table in
-- this codebase), not the enforcement point for the worker pool itself.
ALTER TABLE queue_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE queue_jobs FORCE ROW LEVEL SECURITY;

CREATE POLICY queue_jobs_isolation ON queue_jobs
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
