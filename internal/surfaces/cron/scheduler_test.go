package cron_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/runs"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/cron"
)

// fakeSchedules answers Due per tenant and records what the scheduler asks of
// storage, so these tests pin the scheduler's own policy (which cron
// expressions are valid, the autonomy default, when the next run is, what
// happens when one step fails) separately from the Postgres adapter.
type fakeSchedules struct {
	due     map[uuid.UUID][]cron.Schedule
	dueErr  map[uuid.UUID]error
	fireErr error

	fired    []cron.Firing
	disabled []uuid.UUID
}

func (f *fakeSchedules) Due(_ context.Context, tenantID uuid.UUID) ([]cron.Schedule, error) {
	return f.due[tenantID], f.dueErr[tenantID]
}

func (f *fakeSchedules) Disable(_ context.Context, _, scheduleID uuid.UUID) error {
	f.disabled = append(f.disabled, scheduleID)
	return nil
}

func (f *fakeSchedules) Fire(_ context.Context, fr cron.Firing) (runs.SealFunc, error) {
	if f.fireErr != nil {
		return nil, f.fireErr
	}
	f.fired = append(f.fired, fr)
	return func(p []byte) ([]byte, []byte, string, error) { return p, p, "fake-key", nil }, nil
}

type recordingStarter struct {
	err  error
	reqs []runs.Request
}

func (s *recordingStarter) StartRun(_ context.Context, req runs.Request) (<-chan runs.Event, error) {
	s.reqs = append(s.reqs, req)
	if s.err != nil {
		return nil, s.err
	}
	ch := make(chan runs.Event)
	close(ch)
	return ch, nil
}

type fakeTenants struct {
	ids []uuid.UUID
	err error
}

func (t fakeTenants) ListTenantIDs(context.Context) ([]uuid.UUID, error) { return t.ids, t.err }

var now = time.Date(2026, 10, 30, 12, 2, 0, 0, time.UTC)

func newScheduler(f *fakeSchedules, st *recordingStarter, tenants ...uuid.UUID) *cron.Scheduler {
	return &cron.Scheduler{
		Schedules: f, Starter: st, Tenants: fakeTenants{ids: tenants},
		CatalogManifestDigest: []byte("catalog-v1"),
		Now:                   func() time.Time { return now },
	}
}

func schedule(expr, autonomy string) cron.Schedule {
	return cron.Schedule{ID: uuid.New(), UserID: uuid.New(), CronExpr: expr, Input: "summarize the inbox", AutonomyLevel: autonomy}
}

func TestRunOnce_AFiringCarriesTheSessionRouteAndTheNextRun(t *testing.T) {
	tenant := uuid.New()
	d := schedule("*/5 * * * *", "")
	f := &fakeSchedules{due: map[uuid.UUID][]cron.Schedule{tenant: {d}}}
	st := &recordingStarter{}

	newScheduler(f, st, tenant).RunOnce(context.Background())

	if len(f.fired) != 1 {
		t.Fatalf("%d firings, want 1", len(f.fired))
	}
	fr := f.fired[0]
	if fr.TenantID != tenant || fr.Schedule.ID != d.ID || fr.SessionID == uuid.Nil {
		t.Errorf("firing = %+v, want this tenant's schedule with a fresh session id", fr)
	}
	if fr.Autonomy != "supervised" {
		t.Errorf("autonomy = %q, want supervised: a schedule with none set must default to the cautious level", fr.Autonomy)
	}
	if !fr.RanAt.Equal(now) {
		t.Errorf("RanAt = %v, want %v", fr.RanAt, now)
	}
	if want := time.Date(2026, 10, 30, 12, 5, 0, 0, time.UTC); !fr.NextRunAt.Equal(want) {
		t.Errorf("NextRunAt = %v, want %v: the next */5 boundary after %v, not after the previous run", fr.NextRunAt, want, now)
	}
	if fr.DataLabel != "internal" || fr.ModelID == "" || len(fr.HarnessDigest) == 0 {
		t.Errorf("firing route = (label %q, model %q, digest %d bytes), want an internal-labelled route with a harness digest", fr.DataLabel, fr.ModelID, len(fr.HarnessDigest))
	}

	if len(st.reqs) != 1 {
		t.Fatalf("%d runs started, want 1", len(st.reqs))
	}
	req := st.reqs[0]
	if req.SessionID != fr.SessionID || req.TenantID != tenant || req.Input != d.Input || req.ModelID != fr.ModelID || req.AutonomyLevel != "supervised" || req.Seal == nil {
		t.Errorf("started %+v, want the fired session, its input, route and the seal Fire returned", req)
	}
}

func TestRunOnce_AnExplicitAutonomyLevelIsKept(t *testing.T) {
	tenant := uuid.New()
	f := &fakeSchedules{due: map[uuid.UUID][]cron.Schedule{tenant: {schedule("@hourly", "autonomous")}}}
	newScheduler(f, &recordingStarter{}, tenant).RunOnce(context.Background())
	if len(f.fired) != 1 || f.fired[0].Autonomy != "autonomous" {
		t.Errorf("firings = %+v, want autonomy autonomous kept", f.fired)
	}
}

func TestRunOnce_TheHarnessDigestFollowsTheToolCatalog(t *testing.T) {
	tenant := uuid.New()
	run := func(manifest string) []byte {
		f := &fakeSchedules{due: map[uuid.UUID][]cron.Schedule{tenant: {schedule("@hourly", "")}}}
		s := newScheduler(f, &recordingStarter{}, tenant)
		s.CatalogManifestDigest = []byte(manifest)
		s.RunOnce(context.Background())
		return f.fired[0].HarnessDigest
	}
	if string(run("catalog-v1")) == string(run("catalog-v2")) {
		t.Error("a session's harness digest did not move when the resident tool catalog did: the tool universe is behavior-bearing config")
	}
}

func TestRunOnce_AnUnparseableScheduleIsDisabledAndNeverFired(t *testing.T) {
	tenant := uuid.New()
	bad, good := schedule("not a cron expression", ""), schedule("@hourly", "")
	f := &fakeSchedules{due: map[uuid.UUID][]cron.Schedule{tenant: {bad, good}}}
	st := &recordingStarter{}

	newScheduler(f, st, tenant).RunOnce(context.Background())

	if len(f.disabled) != 1 || f.disabled[0] != bad.ID {
		t.Errorf("disabled %v, want exactly the bad schedule %s", f.disabled, bad.ID)
	}
	if len(f.fired) != 1 || f.fired[0].Schedule.ID != good.ID {
		t.Errorf("fired %+v, want only the good schedule: one bad row must not stop the rest", f.fired)
	}
}

func TestRunOnce_AFailureToFireStartsNoRun(t *testing.T) {
	tenant := uuid.New()
	f := &fakeSchedules{due: map[uuid.UUID][]cron.Schedule{tenant: {schedule("@hourly", "")}}, fireErr: errors.New("db down")}
	st := &recordingStarter{}
	newScheduler(f, st, tenant).RunOnce(context.Background())
	if len(st.reqs) != 0 {
		t.Errorf("%d runs started though the session and the schedule's advance never committed: that run would repeat on the next poll", len(st.reqs))
	}
}

func TestRunOnce_AFailedStartDoesNotStopTheOtherSchedules(t *testing.T) {
	tenant := uuid.New()
	f := &fakeSchedules{due: map[uuid.UUID][]cron.Schedule{tenant: {schedule("@hourly", ""), schedule("@daily", "")}}}
	st := &recordingStarter{err: errors.New("provider unavailable")}
	newScheduler(f, st, tenant).RunOnce(context.Background())
	if len(f.fired) != 2 || len(st.reqs) != 2 {
		t.Errorf("fired %d and attempted %d starts, want 2 and 2", len(f.fired), len(st.reqs))
	}
}

func TestRunOnce_OneTenantsFailureDoesNotStopTheOthers(t *testing.T) {
	broken, healthy := uuid.New(), uuid.New()
	f := &fakeSchedules{
		due:    map[uuid.UUID][]cron.Schedule{healthy: {schedule("@hourly", "")}},
		dueErr: map[uuid.UUID]error{broken: errors.New("rls misconfigured")},
	}
	st := &recordingStarter{}
	newScheduler(f, st, broken, healthy).RunOnce(context.Background())
	if len(f.fired) != 1 || f.fired[0].TenantID != healthy {
		t.Errorf("fired %+v, want the healthy tenant's schedule despite the broken tenant before it", f.fired)
	}
}

func TestRunOnce_WithNoTenantListNothingIsFired(t *testing.T) {
	f := &fakeSchedules{}
	s := newScheduler(f, &recordingStarter{})
	s.Tenants = fakeTenants{err: errors.New("admin connection down")}
	s.RunOnce(context.Background())
	if len(f.fired) != 0 {
		t.Errorf("fired %+v though the tenant list failed", f.fired)
	}
}
