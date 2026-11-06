package runctl

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Checkpoint is the artifact README task 6.3 names: a durable, denormalized
// POINTER into the state a crash-recovering resume needs fast, never the
// source of truth for any one field (migrations/0013_checkpoints.sql's own doc
// comment) — the events, claims, approvals and cost rows it points at still
// are. Deleting a checkpoint therefore costs correctness (unlike a snapshot,
// whose whole point is that deleting it costs nothing but hydration time):
// Resume falls back to a full structural replay when none exists, but loses the
// fast "is there an unresolved claim I must not blindly run past" answer.
type Checkpoint struct {
	CheckpointID          uuid.UUID
	TenantID              uuid.UUID
	SessionID             uuid.UUID
	CoveredSeq            int64
	OpenClaimID           *uuid.UUID
	HeldReservationID     *uuid.UUID
	SandboxHandle         string
	PendingApprovalDigest []byte
	ProviderRequestID     string
	OpenDelegations       []uuid.UUID
	HarnessDigest         []byte
	CreatedAt             time.Time
}

// SaveCheckpoint records where a session stands once a worker has finished
// driving it (README task 6.3): the last event it covers, the oldest claim
// still in flight and the harness digest it was pinned to. It is called by the
// queue worker after each leased job returns — whether the run completed,
// suspended or errored — and never by the kernel loop, which keeps the writer
// off the loop's own hot path. Checkpoints are append-only; the latest one is
// the current one.
func (c *Control) SaveCheckpoint(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	return c.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		sess, err := tx.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		claims, err := tx.ListInFlightClaims(ctx, sessionID)
		if err != nil {
			return err
		}
		var openClaim *uuid.UUID
		if len(claims) > 0 {
			openClaim = &claims[0].ClaimID
		}
		history, err := tx.ListEvents(ctx, sessionID)
		if err != nil {
			return err
		}
		var coveredSeq int64
		if len(history) > 0 {
			coveredSeq = history[len(history)-1].Seq
		}
		if _, err := tx.SaveCheckpoint(ctx, Checkpoint{
			TenantID: tenantID, SessionID: sessionID, CoveredSeq: coveredSeq,
			OpenClaimID: openClaim, HarnessDigest: sess.HarnessDigest,
		}); err != nil {
			return fmt.Errorf("runctl: save checkpoint: %w", err)
		}
		return nil
	})
}
