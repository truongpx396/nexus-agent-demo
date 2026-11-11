package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Unresumed is an approval that was decided while its run was suspended, and
// whose run is still suspended: the decision was recorded but nothing resumed it.
type Unresumed struct {
	TenantID   uuid.UUID
	SessionID  uuid.UUID
	ApprovalID uuid.UUID
}

// unresumedGrace is how long a decision may stand before it is called unresumed,
// so recovery does not race the queue job that was enqueued with the decision.
const unresumedGrace = "1 minute"

// unresumedWindow bounds how far back recovery looks. A run suspended for another
// reason (a delegation) also matches an old, long-resumed approval, which
// ResumeDecided then finds already answered, so the window only keeps that
// harmless scan short.
const unresumedWindow = "7 days"

// ListUnresumed finds approvals decided but never resumed. The queue job that
// should resume one is enqueued after the decision commits, so a Redis outage or
// a crash between the two leaves a run suspended with nothing to wake it.
//
// It is cross-tenant, so pool must be an administrative connection, not the
// RLS-restricted runtime role. A suspended session is the evidence that nothing
// resumed it: a resume sets the status to running before it does anything else.
func ListUnresumed(ctx context.Context, pool *pgxpool.Pool) ([]Unresumed, error) {
	rows, err := pool.Query(ctx, `
		SELECT a.tenant_id, a.session_id, a.approval_id
		FROM approvals a
		JOIN sessions s ON s.session_id = a.session_id
		WHERE a.status IN ('granted', 'granted_modified', 'denied')
		  AND s.status = 'suspended'
		  AND a.decided_at < now() - interval '`+unresumedGrace+`'
		  AND a.decided_at > now() - interval '`+unresumedWindow+`'`)
	if err != nil {
		return nil, fmt.Errorf("oversight: list unresumed approvals: %w", err)
	}
	defer rows.Close()
	var out []Unresumed
	for rows.Next() {
		var u Unresumed
		if err := rows.Scan(&u.TenantID, &u.SessionID, &u.ApprovalID); err != nil {
			return nil, fmt.Errorf("oversight: scan unresumed approval: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
