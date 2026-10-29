package cron

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/runs"
)

// Schedule is one cron_schedules row the scheduler acts on.
type Schedule struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	CronExpr      string
	Input         string
	AutonomyLevel string
}

// Firing is everything needed to turn one due Schedule into a run: the session
// the scheduler decided on, and the two times that move the schedule along.
type Firing struct {
	TenantID  uuid.UUID
	Schedule  Schedule
	SessionID uuid.UUID

	// The session's route and harness, decided by the scheduler (the same
	// route, digest and autonomy default every other surface uses).
	HarnessDigest []byte
	DataLabel     string
	ModelID       string
	RouteReason   map[string]string
	Autonomy      string

	// RanAt and NextRunAt become the schedule's last_run_at and next_run_at.
	RanAt     time.Time
	NextRunAt time.Time
}

// ScheduleStore is the scheduler's storage. Every method runs in the named
// tenant's own scope — there is no cross-tenant query through the runtime
// pool (TenantLister's doc comment).
type ScheduleStore interface {
	// Due returns the tenant's enabled schedules whose next run has come (or
	// has never been set).
	Due(ctx context.Context, tenantID uuid.UUID) ([]Schedule, error)

	// Disable turns a schedule off for good, which is what happens to one whose
	// cron expression cannot be parsed.
	Disable(ctx context.Context, tenantID, scheduleID uuid.UUID) error

	// Fire creates the session f describes, mints its key, and advances the
	// schedule, as ONE atomic step: a schedule that advanced always has its
	// session, and a session never exists for a tick that did not advance. That
	// is the exactly-once tick, and it is why this is a single method rather
	// than a create-session call followed by an update. It returns the seal
	// function for the new session's key, for the run to start with.
	Fire(ctx context.Context, f Firing) (runs.SealFunc, error)
}
