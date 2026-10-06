-- Inbound delivery dedup (docs/build-phases.md Phase 18, review finding
-- #2): the cheap first line of defense a provider-facing webhook needs
-- against its own retry behavior ("Telegram/Zalo webhooks retry by
-- design") — every conversational surface's dispatch() used to have
-- nothing keyed on the provider's own delivery id (Telegram's update_id,
-- email's Message-ID; Zalo's msg_id wasn't even parsed), so a redelivered
-- update reran dispatch's session lookup and could start a second run for
-- input already accepted.
--
-- The UNIQUE constraint IS the mechanism: store.ClaimInboundDelivery does
-- one INSERT ... ON CONFLICT DO NOTHING; the first caller for a given
-- (tenant_id, surface_id, delivery_id) triple gets the row and proceeds,
-- every later caller for the SAME triple — a genuine provider retry, or
-- two goroutines racing the same delivery across nexusd pods, since this
-- table (like every other) is the shared source of truth, not per-process
-- memory — finds it already claimed and treats the delivery as handled.
-- session_key rides along only for operator debugging (which conversation
-- a delivery belonged to); it carries no constraint of its own.
CREATE TABLE inbound_deliveries (
    tenant_id     uuid NOT NULL REFERENCES tenants (tenant_id),
    surface_id    text NOT NULL,
    delivery_id   text NOT NULL,
    session_key   text NOT NULL,
    received_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, surface_id, delivery_id)
);

ALTER TABLE inbound_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE inbound_deliveries FORCE ROW LEVEL SECURITY;

CREATE POLICY inbound_deliveries_isolation ON inbound_deliveries
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
