-- Per-tenant admitted MCP server registry (README Phase 11, task 11.1): the
-- catalog a remote MCP server's tools are admitted THROUGH — "admitted
-- through the ordinary identity (#13), manifest (#14), and descriptor-scan
-- (#15) path" only means something if there is a durable, per-tenant record
-- of which servers a tenant has actually admitted, distinct from "any URL
-- the model happens to mention." internal/surfaces/mcp.Resolver (the
-- Pipeline.DynamicResolver implementation, internal/tools/pipeline.go)
-- refuses to resolve any ref whose server isn't a row here with
-- status='admitted' — fail closed, same discipline as a skill bundle's own
-- admission gate (migrations/0008... skills) or a tool's own
-- AdmissionStatus (internal/tools/admit.go).
CREATE TABLE mcp_servers (
    server_id               uuid PRIMARY KEY,
    tenant_id                 uuid NOT NULL REFERENCES tenants (tenant_id),
    name                        text NOT NULL,   -- the "{server}" in the qualified ref mcp/{server}/{tool}@{version}
    base_url                      text NOT NULL,
    auth_kind                       text NOT NULL DEFAULT 'none',
    sealed_static_token               bytea,        -- set only when auth_kind = 'bearer_static'
    key_id                               uuid REFERENCES encryption_keys (key_id),
    oauth_provider                        text,        -- set only when auth_kind = 'oauth_connector'; matches oauth_connections.provider
    status                                   text NOT NULL DEFAULT 'pending',
    created_at                                 timestamptz NOT NULL DEFAULT now(),

    UNIQUE (tenant_id, name),
    CHECK (auth_kind IN ('none', 'bearer_static', 'oauth_connector')),
    CHECK (status IN ('pending', 'admitted', 'disabled'))
);

CREATE INDEX mcp_servers_tenant_idx ON mcp_servers (tenant_id);

ALTER TABLE mcp_servers ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_servers FORCE ROW LEVEL SECURITY;

CREATE POLICY mcp_servers_isolation ON mcp_servers
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
