package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent/internal/eventlog"
)

// claimAwaitingInput atomically flips a session from awaiting_input to running
// and reports whether this caller made the flip. It is a compare-and-set in one
// statement: the UPDATE takes the row lock, so a concurrent caller blocks until
// the winner commits, then re-checks the WHERE clause, sees running, and loses.
// A check-then-write built from GetSession and SetSessionStatus has no such
// guarantee — GetSession takes no lock, so two callers can both read
// awaiting_input.
func claimAwaitingInput(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (bool, error) {
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

// listStaleAwaitingInput returns every session in awaiting_input whose
// updated_at is older than olderThan — the idle-conversation sweep's own query
// (runctl.Control.SweepIdle). RLS-scoped like every read here: called once per
// tenant, inside that tenant's own transaction.
func listStaleAwaitingInput(ctx context.Context, tx pgx.Tx, olderThan time.Time) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx,
		`SELECT session_id FROM sessions WHERE status = $1 AND updated_at < $2`,
		eventlog.SessionStatusAwaitingInput, olderThan,
	)
	if err != nil {
		return nil, fmt.Errorf("list stale awaiting-input sessions: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stale awaiting-input session id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
