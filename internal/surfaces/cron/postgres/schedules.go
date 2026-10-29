// Package postgres is the Postgres adapter for cron.ScheduleStore
// (migrations/0021_cron_schedules.sql).
package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/cron"
)

// Schedules implements cron.ScheduleStore. Every call runs in the named
// tenant's own scope: the runtime pool (nexus_app) cannot make a cross-tenant
// query, so the scheduler lists tenants separately and asks per tenant.
type Schedules struct {
	st   *store.Store
	keys *crypto.KeyStore
}

var _ cron.ScheduleStore = (*Schedules)(nil)

// New returns Schedules over st; keys mints the key of each session it creates.
func New(st *store.Store, keys *crypto.KeyStore) *Schedules { return &Schedules{st: st, keys: keys} }

func (s *Schedules) Due(ctx context.Context, tenantID uuid.UUID) ([]cron.Schedule, error) {
	var due []cron.Schedule
	err := s.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT schedule_id, user_id, cron_expr, input, autonomy_level
			 FROM cron_schedules WHERE enabled AND (next_run_at IS NULL OR next_run_at <= now())`,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d cron.Schedule
			if err := rows.Scan(&d.ID, &d.UserID, &d.CronExpr, &d.Input, &d.AutonomyLevel); err != nil {
				return err
			}
			due = append(due, d)
		}
		return rows.Err()
	})
	return due, err
}

func (s *Schedules) Disable(ctx context.Context, tenantID, scheduleID uuid.UUID) error {
	return s.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE cron_schedules SET enabled = false WHERE schedule_id = $1`, scheduleID)
		return err
	})
}

// Fire mints the session's key, creates the session and advances the schedule
// in one transaction, so the tick is exactly-once: if any step fails nothing
// is kept, and the schedule is still due on the next poll.
func (s *Schedules) Fire(ctx context.Context, f cron.Firing) (runs.SealFunc, error) {
	var dek crypto.DEK
	err := s.st.InTenantTx(ctx, f.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		dek, derr = s.keys.NewDEK(ctx, tx, f.TenantID)
		if derr != nil {
			return derr
		}
		if err := store.CreateSession(ctx, tx, store.Session{
			SessionID:     f.SessionID,
			SessionKey:    f.SessionID.String(),
			TenantID:      f.TenantID,
			SurfaceID:     "cron",
			UserID:        f.Schedule.UserID,
			AgentVersion:  1,
			HarnessDigest: f.HarnessDigest,
			DataLabel:     f.DataLabel,
			RouteModelID:  f.ModelID,
			RouteReason:   f.RouteReason,
			AutonomyLevel: f.Autonomy,
		}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE cron_schedules SET last_run_at = $1, next_run_at = $2 WHERE schedule_id = $3`,
			f.RanAt, f.NextRunAt, f.Schedule.ID,
		)
		return err
	})
	if err != nil {
		return nil, err
	}
	return runs.SealFor(dek, f.TenantID, f.SessionID), nil
}
