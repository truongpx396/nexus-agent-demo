package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PinSession writes the plan version a session is a run of. plan_sessions is
// plan's own table, and a session may be pinned once.
func (t *txn) PinSession(ctx context.Context, tenantID, sessionID, planID uuid.UUID, version int) error {
	if _, err := t.Tx().Exec(ctx,
		`INSERT INTO plan_sessions (session_id, tenant_id, plan_id, plan_version) VALUES ($1, $2, $3, $4)`,
		sessionID, tenantID, planID, version,
	); err != nil {
		return fmt.Errorf("plan: pin session %s to plan %s version %d: %w", sessionID, planID, version, err)
	}
	return nil
}

// PinnedPlan reads the pin; ok=false is a session that is not plan-driven.
func (t *txn) PinnedPlan(ctx context.Context, sessionID uuid.UUID) (planID uuid.UUID, version int, ok bool, err error) {
	err = t.Tx().QueryRow(ctx, `SELECT plan_id, plan_version FROM plan_sessions WHERE session_id = $1`, sessionID).Scan(&planID, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, false, nil
	}
	if err != nil {
		return uuid.Nil, 0, false, fmt.Errorf("plan: read pin of session %s: %w", sessionID, err)
	}
	return planID, version, true, nil
}
