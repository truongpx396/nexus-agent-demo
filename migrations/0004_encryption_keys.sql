-- Per-tenant content-encryption keys (DEKs), each wrapped under the
-- operator-held KEK (internal/crypto). Erasure in Phase 5 sets status to
-- 'shredded' and destroys nothing else — the row and its wrapped ciphertext
-- stay, permanently useless, which is the point: it lets a verifier prove a
-- key was destroyed rather than merely forgotten (FR-080).
CREATE TABLE encryption_keys (
    key_id          uuid PRIMARY KEY,
    tenant_id       uuid NOT NULL REFERENCES tenants (tenant_id),
    wrapped_dek     bytea NOT NULL,
    status          text NOT NULL DEFAULT 'active',
    created_at      timestamptz NOT NULL DEFAULT now(),
    shredded_at     timestamptz
);

CREATE INDEX encryption_keys_tenant_idx ON encryption_keys (tenant_id);

-- See 0001_tenants.sql: the real enforcement point is nexus_app
-- (0000_app_role.sql) — nexus, the migration/owner role, is a superuser and
-- bypasses RLS regardless of ENABLE/FORCE.
ALTER TABLE encryption_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE encryption_keys FORCE ROW LEVEL SECURITY;

CREATE POLICY encryption_keys_isolation ON encryption_keys
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
