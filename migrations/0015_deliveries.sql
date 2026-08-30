-- The delivery outbox (README task 7.14, pattern #50): an event is appended
-- BEFORE a surface attempts to send it, at-least-once, idempotent on
-- (session_id, seq, surface_id, recipient) — a retry of the exact same
-- triple is a no-op via the UNIQUE constraint below, not a duplicate send.
-- failed_permanent (after the retry cap) stays a distinct, queryable status
-- from "nobody has attempted this yet" (pending) — the whole point of task
-- 7.14's own wording.
CREATE TABLE deliveries (
    delivery_id    uuid PRIMARY KEY,
    tenant_id      uuid NOT NULL REFERENCES tenants (tenant_id),
    session_id     uuid NOT NULL REFERENCES sessions (session_id),
    seq            bigint NOT NULL,
    surface_id     text NOT NULL,
    recipient      text NOT NULL,
    status         text NOT NULL DEFAULT 'pending',
    attempt_count  int NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    delivered_at   timestamptz,

    CHECK (status IN ('pending', 'delivered', 'failed', 'failed_permanent', 'suppressed')),
    UNIQUE (session_id, seq, surface_id, recipient)
);

CREATE INDEX deliveries_pending_idx ON deliveries (tenant_id) WHERE status IN ('pending', 'failed');

ALTER TABLE deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE deliveries FORCE ROW LEVEL SECURITY;

CREATE POLICY deliveries_isolation ON deliveries
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
