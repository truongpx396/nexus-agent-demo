package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
)

// saveCheckpoint appends a new checkpoint row — checkpoints are append-only
// like every other durable record here; LatestCheckpoint is how a caller
// finds the current one. Called by internal/queue's worker after each
// leased job returns (whether the run completed, suspended, or errored),
// never by the kernel loop itself (kernel/types.go's own import allowlist
// already names internal/reliability, not internal/queue — the checkpoint
// writer is deliberately outside the loop's own hot path).
func saveCheckpoint(ctx context.Context, tx pgx.Tx, c runctl.Checkpoint) (runctl.Checkpoint, error) {
	delegations, err := json.Marshal(c.OpenDelegations)
	if err != nil {
		return runctl.Checkpoint{}, fmt.Errorf("marshal open_delegations: %w", err)
	}
	if c.CheckpointID == uuid.Nil {
		c.CheckpointID = uuid.New()
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO checkpoints (
			checkpoint_id, tenant_id, session_id, covered_seq, open_claim_id,
			held_reservation_id, sandbox_handle, pending_approval_digest,
			provider_request_id, open_delegations, harness_digest
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING created_at`,
		c.CheckpointID, c.TenantID, c.SessionID, c.CoveredSeq, c.OpenClaimID,
		c.HeldReservationID, nullIfEmpty(c.SandboxHandle), c.PendingApprovalDigest,
		nullIfEmpty(c.ProviderRequestID), delegations, c.HarnessDigest,
	).Scan(&c.CreatedAt)
	if err != nil {
		return runctl.Checkpoint{}, fmt.Errorf("save checkpoint for session %s: %w", c.SessionID, err)
	}
	return c, nil
}

// latestCheckpoint returns sessionID's most recent checkpoint, if any.
func latestCheckpoint(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (runctl.Checkpoint, bool, error) {
	row := tx.QueryRow(ctx, `
		SELECT checkpoint_id, tenant_id, session_id, covered_seq, open_claim_id,
		       held_reservation_id, sandbox_handle, pending_approval_digest,
		       provider_request_id, open_delegations, harness_digest, created_at
		FROM checkpoints WHERE session_id = $1 ORDER BY created_at DESC LIMIT 1`, sessionID)

	var c runctl.Checkpoint
	var sandboxHandle, providerRequestID *string
	var delegations []byte
	err := row.Scan(
		&c.CheckpointID, &c.TenantID, &c.SessionID, &c.CoveredSeq, &c.OpenClaimID,
		&c.HeldReservationID, &sandboxHandle, &c.PendingApprovalDigest,
		&providerRequestID, &delegations, &c.HarnessDigest, &c.CreatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return runctl.Checkpoint{}, false, nil
		}
		return runctl.Checkpoint{}, false, fmt.Errorf("latest checkpoint for session %s: %w", sessionID, err)
	}
	if sandboxHandle != nil {
		c.SandboxHandle = *sandboxHandle
	}
	if providerRequestID != nil {
		c.ProviderRequestID = *providerRequestID
	}
	if len(delegations) > 0 {
		if err := json.Unmarshal(delegations, &c.OpenDelegations); err != nil {
			return runctl.Checkpoint{}, false, fmt.Errorf("unmarshal open_delegations: %w", err)
		}
	}
	return c, true, nil
}
