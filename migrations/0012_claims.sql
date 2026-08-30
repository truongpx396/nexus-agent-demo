-- Write-ahead idempotency claims (README task 6.6): before a non-read-only
-- tool's effect leaves the process (internal/tools/pipeline.go's finishCall,
-- step 13), a claim is durably recorded in_flight — keyed by
-- (session_id, canonical_digest), the SAME digest task 3.5 already names as
-- "one artifact, three jobs" (approval binding, idempotency key, step-9a
-- re-verification); this table is that third job made real. A claim is
-- resolved by a probe or a human (internal/runctl.ResolveClaim) — NEVER by
-- re-executing the tool and never by silently discarding the ambiguity.
CREATE TABLE claims (
    claim_id           uuid PRIMARY KEY,
    tenant_id          uuid NOT NULL REFERENCES tenants (tenant_id),
    session_id         uuid NOT NULL REFERENCES sessions (session_id),
    tool_id            text NOT NULL,
    canonical_digest   bytea NOT NULL,
    status             text NOT NULL DEFAULT 'in_flight', -- in_flight | completed | abandoned
    reason             text,                       -- populated on Complete/Resolve
    created_at         timestamptz NOT NULL DEFAULT now(),
    resolved_at        timestamptz,

    CHECK (status IN ('in_flight', 'completed', 'abandoned')),
    UNIQUE (session_id, canonical_digest)
);

CREATE INDEX claims_session_idx ON claims (session_id);
CREATE INDEX claims_in_flight_idx ON claims (tenant_id) WHERE status = 'in_flight';

ALTER TABLE claims ENABLE ROW LEVEL SECURITY;
ALTER TABLE claims FORCE ROW LEVEL SECURITY;

CREATE POLICY claims_isolation ON claims
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
