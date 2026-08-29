package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Session status values. SessionStatusSuspended is Phase 3's: the
// permission chain resolved ASK with no standing scope and no oversight
// service yet to act on it (kernel/loop.go's suspendForApproval), so the
// run pauses here rather than terminating or continuing. It is not a
// terminal status — Phase 5's internal/oversight owns turning the
// EventApprovalRequested that produced it into a real decision, and Phase
// 6's internal/runctl + checkpoint own resuming the run from it; this phase
// only ships the pause itself.
const (
	SessionStatusQueued    = "queued" // the schema default; a session that has never had Run() start
	SessionStatusRunning   = "running"
	SessionStatusSuspended = "suspended"
	SessionStatusCompleted = "completed"
	SessionStatusFailed    = "failed"
)

// UpdateSessionStatus writes sessions.status (and terminal_reason, once the
// run has one) — always called in the same transaction as the event that
// justifies the change (kernel/loop.go), which is what keeps this a
// same-transaction projection rather than a second source of truth (see the
// Session doc comment above).
func UpdateSessionStatus(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID, status string, terminalReason *string) error {
	_, err := tx.Exec(ctx,
		`UPDATE sessions SET status = $2, terminal_reason = $3 WHERE session_id = $1`,
		sessionID, status, terminalReason,
	)
	if err != nil {
		return fmt.Errorf("update session %s status: %w", sessionID, err)
	}
	return nil
}
