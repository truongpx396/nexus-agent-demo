-- Tenant is the first dimension of every keyed entity (docs/constitution.md,
-- Principle VI). This table is the root registry: every other tenant-scoped
-- table's tenant_id is a foreign key into it.
CREATE TABLE tenants (
    tenant_id           uuid PRIMARY KEY,
    name                text NOT NULL,
    region              text NOT NULL DEFAULT 'local',
    data_label_default  text NOT NULL DEFAULT 'internal',
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- RLS even on the root registry, for the same reason every other
-- tenant-scoped table gets it: a connection scoped to one tenant must never
-- be able to enumerate another tenant's row, even a mostly-empty one.
-- FORCE additionally subjects this table's OWNER to the policy — but the
-- owner here (nexus, the migration role) is a superuser, and RLS is
-- unconditionally bypassed for a superuser regardless of ENABLE/FORCE.
-- The actual enforcement point is 0000_app_role.sql: nexusd's runtime path
-- connects as nexus_app, an ordinary role with no bypass, for which this
-- policy is the only way in or out.
ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE ROW LEVEL SECURITY;

CREATE POLICY tenants_isolation ON tenants
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
