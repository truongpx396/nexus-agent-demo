package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// UpdateSessionStatus writes sessions.status (and terminal_reason, once the
// run has one), plus updated_at — always called in the same transaction as
// the event that justifies the change (kernel/loop.go), which is what keeps
// this a same-transaction projection rather than a second source of truth
// (see the Session doc comment above). updated_at is the idle-conversation
// sweep's own signal (cmd/nexusd/background.go) for how long a session has
// sat in SessionStatusAwaitingInput with nobody answering.
func UpdateSessionStatus(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID, status string, terminalReason *string) error {
	_, err := tx.Exec(ctx,
		`UPDATE sessions SET status = $2, terminal_reason = $3, updated_at = now() WHERE session_id = $1`,
		sessionID, status, terminalReason,
	)
	if err != nil {
		return fmt.Errorf("update session %s status: %w", sessionID, err)
	}
	return nil
}

// ClaimAwaitingInput atomically flips a session from awaiting_input to
// running and reports whether this caller made the flip. It is a
// compare-and-set in one statement: the UPDATE takes the row lock, so a
// concurrent caller blocks until the winner commits, then re-checks the
// WHERE clause, sees running, and loses. A check-then-write built from
// GetSession and UpdateSessionStatus has no such guarantee — GetSession
// takes no lock, so two callers can both read awaiting_input.
func ClaimAwaitingInput(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE sessions SET status = $2, terminal_reason = NULL, updated_at = now()
		 WHERE session_id = $1 AND status = $3`,
		sessionID, eventlog.SessionStatusRunning, eventlog.SessionStatusAwaitingInput,
	)
	if err != nil {
		return false, fmt.Errorf("claim session %s from awaiting_input: %w", sessionID, err)
	}
	return tag.RowsAffected() == 1, nil
}
