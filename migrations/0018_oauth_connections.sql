-- Per-user OAuth token vault (README Phase 11, task 11.2): one row per
-- (tenant, user, provider) authorization-code grant. sealed_access_token /
-- sealed_refresh_token are envelope-encrypted under the tenant's own DEK
-- (internal/crypto, the same encryption_keys row every other sealed payload
-- in this system uses) — AAD-bound to (tenant_id, user_id|provider) rather
-- than (tenant_id, session_id): a connection is not scoped to any one
-- session, it outlives every run that ever uses it. There is no unsealed
-- token anywhere in this table, in an event payload, in a log line, or in a
-- span — internal/connectors.Token is the ONLY place a caller ever sees a
-- live token, and only from inside a connector tool's own Call (README task
-- 11's own acceptance criterion: no live token in any payload/log/span).
CREATE TABLE oauth_connections (
    connection_id          uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES tenants (tenant_id),
    user_id                  uuid NOT NULL,
    provider                  text NOT NULL,
    sealed_access_token         bytea NOT NULL,
    sealed_refresh_token          bytea,
    key_id                          uuid NOT NULL REFERENCES encryption_keys (key_id),
    scope                             text,
    expires_at                         timestamptz,
    created_at                          timestamptz NOT NULL DEFAULT now(),
    updated_at                           timestamptz NOT NULL DEFAULT now(),

    UNIQUE (tenant_id, user_id, provider)
);

CREATE INDEX oauth_connections_tenant_idx ON oauth_connections (tenant_id);

ALTER TABLE oauth_connections ENABLE ROW LEVEL SECURITY;
ALTER TABLE oauth_connections FORCE ROW LEVEL SECURITY;

CREATE POLICY oauth_connections_isolation ON oauth_connections
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- admitted_connector_providers (README's "connectors MUST attach only
-- through the vetted, per-tenant ... catalog", docs/constitution.md): the
-- same config-not-forks jsonb-admitted-set pattern admitted_skill_ids
-- already established for skill bundles, applied to OAuth providers. A
-- tenant with no row in tenant_configs (internal/config's own "absent means
-- defaults" rule) admits none — connectors are opt-in, never on by default.
ALTER TABLE tenant_configs ADD COLUMN admitted_connector_providers jsonb NOT NULL DEFAULT '[]';
