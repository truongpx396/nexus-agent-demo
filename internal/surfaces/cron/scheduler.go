// Package cron is the scheduler surface (README Phase 11, task 11.6): a
// synthetic principal_kind=scheduler submits a run on a fixed schedule
// through the ordinary session-creation-then-StartRun sequence every
// surface uses — no new admission path. Zero kernel change.
package cron

import (
	"context"
	"time"

	"github.com/google/uuid"
	robfigcron "github.com/robfig/cron/v3"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/harness"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
)

// TenantLister lists every tenant this deployment knows about — backed by
// a SEPARATE admin-role connection (nexus_app is NOSUPERUSER NOBYPASSRLS,
// migrations/0000_app_role.sql; there is no cross-tenant query through the
// ordinary pool), the same pattern cmd/nexusd's own listTenantIDs already
// uses for the anchor loop. This scheduler never queries cron_schedules
// itself outside a per-tenant ScheduleStore call — see RunOnce's own doc
// comment.
type TenantLister interface {
	ListTenantIDs(ctx context.Context) ([]uuid.UUID, error)
}

// Scheduler polls every tenant's cron_schedules for due rows and submits a
// run for each — one goroutine, started alongside cmd/nexusd's other
// background loops (startAnchorLoop).
type Scheduler struct {
	Schedules             ScheduleStore
	Starter               runs.Starter
	Tenants               TenantLister
	CatalogManifestDigest []byte

	// PollInterval is how often runOnce is called; defaults to 30s.
	PollInterval time.Duration

	// Now is the clock; nil means time.Now. A schedule's next run is computed
	// from it, so a test needs to be able to move it.
	Now func() time.Time
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
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
			s.RunOnce(ctx)
		}
	}
}

// RunOnce is one poll pass (Run calls it on a ticker): list tenants (admin connection), then for
// EACH tenant, an ordinary tenant-scoped query for its own due rows — this
// is never a single cross-tenant query, because the runtime pool
// (nexus_app) cannot make one (TenantLister's own doc comment).
func (s *Scheduler) RunOnce(ctx context.Context) {
	tenantIDs, err := s.Tenants.ListTenantIDs(ctx)
	if err != nil {
		log.Error().Err(err).Msg("cron: list tenants")
		return
	}
	for _, tenantID := range tenantIDs {
		s.runTenant(ctx, tenantID)
	}
}

func (s *Scheduler) runTenant(ctx context.Context, tenantID uuid.UUID) {
	due, err := s.Schedules.Due(ctx, tenantID)
	if err != nil {
		log.Error().Err(err).Any("tenant_id", tenantID).Msg("cron: list due schedules")
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
func (s *Scheduler) fire(ctx context.Context, tenantID uuid.UUID, d Schedule) {
	sched, err := robfigcron.ParseStandard(d.CronExpr)
	if err != nil {
		log.Error().Err(err).Any("schedule_id", d.ID).Msg("cron: unparseable cron_expr, disabling schedule")
		s.disable(ctx, tenantID, d.ID)
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

	now := s.now().UTC()
	seal, err := s.Schedules.Fire(ctx, Firing{
		TenantID: tenantID, Schedule: d, SessionID: sessionID,
		HarnessDigest: digest, DataLabel: string(provider.DataLabelInternal),
		ModelID: route.ModelID, RouteReason: route.Reason, Autonomy: autonomy,
		RanAt: now, NextRunAt: sched.Next(now),
	})
	if err != nil {
		log.Error().Err(err).Any("schedule_id", d.ID).Msg("cron: create session for due schedule")
		return
	}

	req := runs.Request{
		SessionID: sessionID, TenantID: tenantID,
		Seal:          seal,
		Input:         d.Input,
		ModelID:       route.ModelID,
		AutonomyLevel: autonomy,
	}
	events, err := s.Starter.StartRun(context.Background(), req)
	if err != nil {
		log.Error().Err(err).Any("schedule_id", d.ID).Msg("cron: start run for due schedule")
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
func drain(events <-chan runs.Event) {
	for range events {
	}
}

func (s *Scheduler) disable(ctx context.Context, tenantID, scheduleID uuid.UUID) {
	if err := s.Schedules.Disable(ctx, tenantID, scheduleID); err != nil {
		log.Error().Err(err).Any("schedule_id", scheduleID).Msg("cron: disable unparseable schedule")
	}
}
