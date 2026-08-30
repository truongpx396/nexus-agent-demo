-- The Checkpoint artifact (README task 6.3) — one of the three state
-- artifacts this phase names. A checkpoint is a durable, denormalized
-- POINTER into the state a crash-recovering resume needs to reason about
-- fast, WITHOUT re-deriving it: it is never the source of truth for any of
-- these fields (the events/claims/approvals/cost_records rows it points at
-- are), the same "projection, never a second source of truth" discipline
-- store/session.go's own status/terminal_reason columns already follow.
CREATE TABLE checkpoints (
    checkpoint_id            uuid PRIMARY KEY,
    tenant_id                uuid NOT NULL REFERENCES tenants (tenant_id),
    session_id                uuid NOT NULL REFERENCES sessions (session_id),
    covered_seq                bigint NOT NULL,          -- the last event seq this checkpoint attests as durably applied
    open_claim_id               uuid,                     -- an in_flight claims row outstanding at checkpoint time, if any
    held_reservation_id         uuid,                     -- an internal/cost.Reservation.ID not yet reconciled at checkpoint time
    sandbox_handle               text,                     -- opaque internal/sandbox container handle, if a sandbox session was live
    pending_approval_digest      bytea,                    -- CanonicalDigest of an outstanding approval, if any
    provider_request_id          text,                     -- an in-flight Provider.Stream request id, if mid-stream at checkpoint time
    open_delegations              jsonb NOT NULL DEFAULT '[]'::jsonb, -- child session ids not yet returned; empty until Phase 8
    harness_digest                bytea NOT NULL,
    created_at                    timestamptz NOT NULL DEFAULT now()
);

-- Resume (internal/runctl) only ever wants the LATEST checkpoint for a
-- session; every row before it is kept for audit/inspection, never mutated
-- (append-only, like every other durable record in this codebase).
CREATE INDEX checkpoints_session_latest_idx ON checkpoints (session_id, created_at DESC);

ALTER TABLE checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE checkpoints FORCE ROW LEVEL SECURITY;

CREATE POLICY checkpoints_isolation ON checkpoints
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- The Snapshot artifact (README task 6.4): a DISPOSABLE projection cache —
-- task 6.4's own acceptance test asserts that deleting every row here
-- changes nothing but hydration time, never the answer. internal/store's
-- ReplayProjection (projection.go) is the slow, always-correct path this
-- table exists only to shortcut.
CREATE TABLE snapshots (
    snapshot_id        uuid PRIMARY KEY,
    tenant_id          uuid NOT NULL REFERENCES tenants (tenant_id),
    session_id         uuid NOT NULL REFERENCES sessions (session_id),
    at_seq             bigint NOT NULL,
    status             text NOT NULL,
    terminal_reason    text,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX snapshots_session_latest_idx ON snapshots (session_id, at_seq DESC);

ALTER TABLE snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE snapshots FORCE ROW LEVEL SECURITY;

CREATE POLICY snapshots_isolation ON snapshots
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
