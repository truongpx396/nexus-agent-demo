// Package cron is the scheduler surface (README Phase 11, task 11.6): a
// synthetic principal_kind=scheduler submits a run on a fixed schedule
// through the ordinary session-creation-then-StartRun sequence every
// surface uses — no new admission path. Zero kernel change.
package cron

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	robfigcron "github.com/robfig/cron/v3"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/harness"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// TenantLister lists every tenant this deployment knows about — backed by
// a SEPARATE admin-role connection (nexus_app is NOSUPERUSER NOBYPASSRLS,
// migrations/0000_app_role.sql; there is no cross-tenant query through the
// ordinary pool), the same pattern cmd/nexusd's own listTenantIDs already
// uses for the anchor loop. This scheduler never queries cron_schedules
// itself outside a per-tenant Store.InTenantTx — see runOnce's own doc
// comment.
type TenantLister interface {
	ListTenantIDs(ctx context.Context) ([]uuid.UUID, error)
}

// Scheduler polls every tenant's cron_schedules for due rows and submits a
// run for each — one goroutine, started alongside cmd/nexusd's other
// background loops (startAnchorLoop).
type Scheduler struct {
	Store                 *store.Store
	KeyStore              *crypto.KeyStore
	Starter               RunStarter
	Tenants               TenantLister
	CatalogManifestDigest []byte

	// PollInterval is how often runOnce is called; defaults to 30s.
	PollInterval time.Duration
}

func (s *Scheduler) pollInterval() time.Duration {
	if s.PollInterval <= 0 {
		return 30 * time.Second
	}
	return s.PollInterval
}

// Run blocks, polling until ctx is cancelled — cmd/nexusd starts this in
// its own goroutine, the same shape startAnchorLoop's own caller uses.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.pollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

// runOnce is one poll pass: list tenants (admin connection), then for
// EACH tenant, an ordinary tenant-scoped query for its own due rows — this
// is never a single cross-tenant query, because the runtime pool
// (nexus_app) cannot make one (TenantLister's own doc comment).
func (s *Scheduler) runOnce(ctx context.Context) {
	tenantIDs, err := s.Tenants.ListTenantIDs(ctx)
	if err != nil {
		slog.Error("cron: list tenants", "error", err)
		return
	}
	for _, tenantID := range tenantIDs {
		s.runTenant(ctx, tenantID)
	}
}

type dueSchedule struct {
	ScheduleID    uuid.UUID
	UserID        uuid.UUID
	CronExpr      string
	Input         string
	AutonomyLevel string
}

func (s *Scheduler) runTenant(ctx context.Context, tenantID uuid.UUID) {
	var due []dueSchedule
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT schedule_id, user_id, cron_expr, input, autonomy_level
			 FROM cron_schedules WHERE enabled AND (next_run_at IS NULL OR next_run_at <= now())`,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d dueSchedule
			if err := rows.Scan(&d.ScheduleID, &d.UserID, &d.CronExpr, &d.Input, &d.AutonomyLevel); err != nil {
				return err
			}
			due = append(due, d)
		}
		return rows.Err()
	})
	if err != nil {
		slog.Error("cron: list due schedules", "error", err, "tenant_id", tenantID)
		return
	}

	for _, d := range due {
		s.fire(ctx, tenantID, d)
	}
}

// fire submits one run for d and advances its next_run_at — the ordinary
// create-session-then-StartRun sequence, resolved as the synthetic
// principal_kind=scheduler capability.PrincipalScheduler pre-declared for
// exactly this (internal/surfaces/capability.go).
func (s *Scheduler) fire(ctx context.Context, tenantID uuid.UUID, d dueSchedule) {
	sched, err := robfigcron.ParseStandard(d.CronExpr)
	if err != nil {
		slog.Error("cron: unparseable cron_expr, disabling schedule", "error", err, "schedule_id", d.ScheduleID)
		s.disable(ctx, tenantID, d.ScheduleID)
		return
	}

	route := provider.Route(provider.DataLabelInternal, provider.DifficultySimple)
	sessionID := uuid.New()
	digest := harness.Digest(harness.Config{
		SystemPromptVersion:   "phase2-v1",
		CatalogManifestDigest: s.CatalogManifestDigest,
		PromptMode:            "phase2-single-shot",
	})
	autonomy := d.AutonomyLevel
	if autonomy == "" {
		autonomy = "supervised"
	}

	now := time.Now().UTC()
	var dek crypto.DEK
	err = s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		dek, derr = s.KeyStore.NewDEK(ctx, tx, tenantID)
		if derr != nil {
			return derr
		}
		if err := store.CreateSession(ctx, tx, store.Session{
			SessionID:     sessionID,
			SessionKey:    sessionID.String(),
			TenantID:      tenantID,
			SurfaceID:     "cron",
			UserID:        d.UserID,
			AgentVersion:  1,
			HarnessDigest: digest,
			DataLabel:     string(provider.DataLabelInternal),
			RouteModelID:  route.ModelID,
			RouteReason:   route.Reason,
			AutonomyLevel: autonomy,
		}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE cron_schedules SET last_run_at = $1, next_run_at = $2 WHERE schedule_id = $3`,
			now, sched.Next(now), d.ScheduleID,
		)
		return err
	})
	if err != nil {
		slog.Error("cron: create session for due schedule", "error", err, "schedule_id", d.ScheduleID)
		return
	}

	req := RunRequest{
		SessionID: sessionID, TenantID: tenantID,
		Seal:          sealFuncFor(dek, tenantID, sessionID),
		Input:         d.Input,
		ModelID:       route.ModelID,
		AutonomyLevel: autonomy,
	}
	events, err := s.Starter.StartRun(context.Background(), req)
	if err != nil {
		slog.Error("cron: start run for due schedule", "error", err, "schedule_id", d.ScheduleID)
		return
	}
	go drain(events)
}

// drain is this surface's own publishUntilDone counterpart — cron has no
// live subscriber to fan out to and no approval-notification channel
// configured (a scheduled run that stops on an approval waits for a human
// on whatever surface actually reviews approvals — REST/web — the same as
// any other run); it only needs to keep the channel draining so
// kernelRunStarter's own goroutine can complete.
func drain(events <-chan RunEvent) {
	for range events {
	}
}

func (s *Scheduler) disable(ctx context.Context, tenantID, scheduleID uuid.UUID) {
	if err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE cron_schedules SET enabled = false WHERE schedule_id = $1`, scheduleID)
		return err
	}); err != nil {
		slog.Error("cron: disable unparseable schedule", "error", err, "schedule_id", scheduleID)
	}
}

func sealFuncFor(dek crypto.DEK, tenantID, sessionID uuid.UUID) SealFunc {
	return func(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", fmt.Errorf("seal event payload: %w", err)
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}
}
