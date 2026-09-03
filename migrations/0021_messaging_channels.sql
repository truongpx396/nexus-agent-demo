-- Per-tenant messaging channel credentials (README Phase 11, tasks 11.4 /
-- 11.5): one shared table for Telegram's bot token, Zalo OA's app secret,
-- and outbound SMTP credentials, since all three are structurally the same
-- fact — "one credential per tenant, sealed under the tenant DEK" — with
-- only the small provider-specific config (SMTP host/port, Zalo OA id, ...)
-- differing. sealed_credential/key_id follow oauth_connections' own shape
-- (migrations/0018) exactly: envelope-encrypted, never placed in an event
-- payload/log/span in plaintext. webhook_secret is deliberately a plain
-- column, not sealed: it is compared against an inbound header/signature on
-- every webhook request (docs/constitution.md's "verify provider
-- authenticity ... before the kernel sees the payload"), a hot path where
-- sealing/unsealing per request would be pure overhead for a value that
-- isn't the credential that actually authenticates OUTBOUND calls.
CREATE TABLE messaging_channels (
    channel_id           uuid PRIMARY KEY,
    tenant_id              uuid NOT NULL REFERENCES tenants (tenant_id),
    kind                      text NOT NULL,
    sealed_credential           bytea NOT NULL,
    key_id                        uuid NOT NULL REFERENCES encryption_keys (key_id),
    webhook_secret                  text,
    config                             jsonb NOT NULL DEFAULT '{}',
    status                                text NOT NULL DEFAULT 'active',
    created_at                              timestamptz NOT NULL DEFAULT now(),

    UNIQUE (tenant_id, kind),
    CHECK (kind IN ('telegram', 'zalo', 'email_smtp')),
    CHECK (status IN ('active', 'disabled'))
);

CREATE INDEX messaging_channels_tenant_idx ON messaging_channels (tenant_id);

ALTER TABLE messaging_channels ENABLE ROW LEVEL SECURITY;
ALTER TABLE messaging_channels FORCE ROW LEVEL SECURITY;

CREATE POLICY messaging_channels_isolation ON messaging_channels
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
