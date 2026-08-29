-- Hash-chained audit receipts (README.md §5, Phase 5, tasks 5.1-5.3):
-- internal/audit.Chain.Append writes one receipt per event durably
-- appended to the log, in the SAME transaction as the event itself
-- (kernel.Kernel.Receipts, wired from cmd/nexusd) — an event without a
-- receipt must never be observable. The chain is over PLAINTEXT DIGESTS,
-- never Payload itself, so a lawful erasure (crypto-shredding the DEK,
-- 0004_encryption_keys.sql) never breaks verification (FR-081).
--
-- seq mirrors events.seq for the same session (one receipt per event, same
-- ordinal) — internal/audit/verify.go compares the two to detect a gap: an
-- event appended with no matching receipt. receipt_seq is a SEPARATE,
-- globally monotonic ordinal (not per-session) purely so anchoring
-- (audit_anchors below) has one simple contiguous range to cover instead of
-- interleaving many per-session sequences.
CREATE TABLE audit_receipts (
    receipt_id      uuid PRIMARY KEY,
    receipt_seq     bigserial NOT NULL,
    tenant_id       uuid NOT NULL REFERENCES tenants (tenant_id),
    session_id      uuid NOT NULL REFERENCES sessions (session_id),
    seq             bigint NOT NULL,        -- mirrors events.seq for (session_id, seq)
    event_id        uuid NOT NULL,
    event_type      text NOT NULL,
    payload_digest  bytea NOT NULL,         -- copied from events.payload_digest; survives shredding
    prev_hash       bytea,                  -- NULL only for a session's first receipt
    hash            bytea NOT NULL,         -- SHA256(prev_hash || tenant || session || seq || event_id || type || payload_digest)
    signature       bytea NOT NULL,         -- Ed25519 signature over hash, from signerd
    signer_key_id   text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),

    UNIQUE (session_id, seq)
);

CREATE UNIQUE INDEX audit_receipts_receipt_seq_idx ON audit_receipts (receipt_seq);
CREATE INDEX audit_receipts_tenant_idx ON audit_receipts (tenant_id, receipt_seq);

ALTER TABLE audit_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_receipts FORCE ROW LEVEL SECURITY;

CREATE POLICY audit_receipts_isolation ON audit_receipts
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- The chain is only as trustworthy as its own immutability — same
-- append-only discipline as events (0003_events.sql).
CREATE TRIGGER audit_receipts_append_only
    BEFORE UPDATE OR DELETE ON audit_receipts
    FOR EACH ROW EXECUTE FUNCTION events_forbid_mutation();

REVOKE UPDATE, DELETE ON audit_receipts FROM nexus_app;

-- Periodic head anchoring outside the transactional writer path (task 5.3):
-- internal/audit/anchor.go runs as a separate scheduled pass (a ticker in
-- cmd/nexusd, or `nexusd verify-chain`), aggregating every receipt written
-- since the last anchor into one signed hash — so a retroactive edit to an
-- already-anchored receipt row is caught by recomputing the aggregate, not
-- only by the per-receipt chain (which an attacker with row-level write
-- access could in principle rewrite consistently from some point forward).
CREATE TABLE audit_anchors (
    anchor_id         uuid PRIMARY KEY,
    tenant_id         uuid NOT NULL REFERENCES tenants (tenant_id),
    from_receipt_seq  bigint NOT NULL,   -- exclusive lower bound (the previous anchor's to_receipt_seq, or 0)
    to_receipt_seq    bigint NOT NULL,   -- inclusive upper bound: max(receipt_seq) covered by this anchor
    hash              bytea NOT NULL,    -- SHA256(prev_anchor_hash || every covered receipt hash, in receipt_seq order)
    signature         bytea NOT NULL,
    signer_key_id     text NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_anchors_tenant_idx ON audit_anchors (tenant_id, to_receipt_seq);

ALTER TABLE audit_anchors ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_anchors FORCE ROW LEVEL SECURITY;

CREATE POLICY audit_anchors_isolation ON audit_anchors
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TRIGGER audit_anchors_append_only
    BEFORE UPDATE OR DELETE ON audit_anchors
    FOR EACH ROW EXECUTE FUNCTION events_forbid_mutation();

REVOKE UPDATE, DELETE ON audit_anchors FROM nexus_app;
