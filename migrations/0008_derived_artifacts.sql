-- Derived artifacts (README.md §5, task 5.4): anything living OUTSIDE the
-- encrypted event log that was derived from a tenant's plaintext content —
-- today, only internal/tools.BlobStore's oversized-result spill files
-- (internal/tools/budget.go's BudgetResult, task 3.13). Erasure
-- (internal/crypto/shred.go) hard-deletes every row for an erased
-- tenant/session in the SAME transaction as the DEK shred, then best-effort
-- unlinks the underlying file; ReconcileDerivedArtifacts is the backstop
-- that proves no row (and no file) outlives its source DEK.
CREATE TABLE derived_artifacts (
    artifact_id    uuid PRIMARY KEY,
    tenant_id      uuid NOT NULL REFERENCES tenants (tenant_id),
    session_id     uuid NOT NULL REFERENCES sessions (session_id),
    kind           text NOT NULL,   -- "blob" today; a seam for memory/condensation-derived artifacts later
    path           text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz
);

CREATE INDEX derived_artifacts_session_idx ON derived_artifacts (session_id);
CREATE INDEX derived_artifacts_tenant_active_idx ON derived_artifacts (tenant_id) WHERE deleted_at IS NULL;

ALTER TABLE derived_artifacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE derived_artifacts FORCE ROW LEVEL SECURITY;

CREATE POLICY derived_artifacts_isolation ON derived_artifacts
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
