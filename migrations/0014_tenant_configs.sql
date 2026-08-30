-- Tenant config store (README Phase 7, internal/config): "config-not-forks
-- onboarding" (pattern #61) made real for the two facts Phase 7's memory and
-- skills packages need per tenant — which skill bundles a tenant has admitted
-- (task 7.6's "the tenant's admitted set"), and how long file-first memory is
-- retained before it drops out of session-start injection (task 7.1's
-- "90-day retention", overridable per tenant rather than a hardcoded
-- constant). One row per tenant; absent means "use the defaults" (never an
-- error — internal/config.Load returns the default row rather than failing
-- when none exists yet).
CREATE TABLE tenant_configs (
    tenant_id            uuid PRIMARY KEY REFERENCES tenants (tenant_id),
    admitted_skill_ids   jsonb NOT NULL DEFAULT '[]',
    memory_retention_days int NOT NULL DEFAULT 90,
    updated_at           timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE tenant_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_configs FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_configs_isolation ON tenant_configs
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
