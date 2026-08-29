package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Session status values this phase produces. "suspended" (an approval or
// input request pending) is Phase 5's — this phase's runs only ever reach
// running and one of the two terminal statuses below.
const (
	SessionStatusQueued    = "queued" // the schema default; a session that has never had Run() start
	SessionStatusRunning   = "running"
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
