package plan_test

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/plan"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// fakeStore is a plan.Store over maps. Like a real transaction it snapshots on
// entry and restores on error, so a transition that fails part-way leaves
// nothing behind — which is what lets these tests pin that a plan's status and
// its governance record only ever change together.
type fakeStore struct {
	mu sync.Mutex

	plans    map[planKey]*planRow
	sessions map[uuid.UUID]eventlog.Session
	events   []appended

	failSignedOff error // makes SetSignedOffBy fail
	failEnable    error // makes SetEnabledPins fail
}

type planKey struct {
	id      uuid.UUID
	version int
}

type planRow struct {
	spec        []byte
	status      plan.Status
	signedOffBy string
	agentPin    int
	modelPin    string
}

type appended struct {
	SessionID uuid.UUID
	Type      eventlog.EventType
	First     bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{plans: map[planKey]*planRow{}, sessions: map[uuid.UUID]eventlog.Session{}}
}

var _ plan.Store = (*fakeStore)(nil)

func (f *fakeStore) InTenant(ctx context.Context, _ uuid.UUID, fn func(ctx context.Context, tx plan.Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	plans := map[planKey]planRow{}
	for k, v := range f.plans {
		plans[k] = *v
	}
	sessions := map[uuid.UUID]eventlog.Session{}
	for k, v := range f.sessions {
		sessions[k] = v
	}
	events := append([]appended(nil), f.events...)
	if err := fn(ctx, &fakeTx{f: f}); err != nil {
		f.plans = map[planKey]*planRow{}
		for k, v := range plans {
			v := v
			f.plans[k] = &v
		}
		f.sessions, f.events = sessions, events
		return err
	}
	return nil
}

func (f *fakeStore) eventTypes() []eventlog.EventType {
	var out []eventlog.EventType
	for _, e := range f.events {
		out = append(out, e.Type)
	}
	return out
}

type fakeTx struct{ f *fakeStore }

var _ plan.Tx = (*fakeTx)(nil)

func (t *fakeTx) Append(_ context.Context, _, sessionID uuid.UUID, typ eventlog.EventType, _ *string, _ *uuid.UUID, _ any) (eventlog.Event, error) {
	// A plan run that never ends would otherwise spin until the test binary's own
	// timeout; fail it at once and say why.
	if len(t.f.events) > 10_000 {
		panic("plan: runaway run - more than 10000 events were appended, a loop is not being bounded")
	}
	t.f.events = append(t.f.events, appended{SessionID: sessionID, Type: typ})
	return eventlog.Event{EventID: uuid.New(), SessionID: sessionID, Type: typ}, nil
}
func (t *fakeTx) AppendFirst(_ context.Context, _, sessionID uuid.UUID, seal kernel.SealFunc, typ eventlog.EventType, payload any) (eventlog.Event, error) {
	if _, _, _, err := seal([]byte(`{}`)); err != nil {
		return eventlog.Event{}, err
	}
	t.f.events = append(t.f.events, appended{SessionID: sessionID, Type: typ, First: true})
	return eventlog.Event{EventID: uuid.New(), SessionID: sessionID, Type: typ}, nil
}

func (t *fakeTx) GetSession(_ context.Context, id uuid.UUID) (eventlog.Session, error) {
	s, ok := t.f.sessions[id]
	if !ok {
		return eventlog.Session{}, errors.New("no such session")
	}
	return s, nil
}
func (t *fakeTx) CreateSession(_ context.Context, s eventlog.Session) error {
	t.f.sessions[s.SessionID] = s
	return nil
}
func (t *fakeTx) SetSessionStatus(_ context.Context, id uuid.UUID, status string, reason *string) error {
	s := t.f.sessions[id]
	s.Status, s.TerminalReason = status, reason
	t.f.sessions[id] = s
	return nil
}
func (t *fakeTx) ListEvents(context.Context, uuid.UUID) ([]eventlog.Event, error) { return nil, nil }
func (t *fakeTx) NewDEK(context.Context, uuid.UUID) (crypto.DEK, error) {
	return crypto.GenerateDEK("k")
}
func (t *fakeTx) Unwrap(context.Context, string) (crypto.DEK, error) { return crypto.GenerateDEK("k") }

func (t *fakeTx) CreateFanoutEnvelope(context.Context, uuid.UUID, uuid.UUID, cost.Money, int) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (t *fakeTx) InsertPlan(_ context.Context, _, planID uuid.UUID, version int, _ string, spec []byte, status plan.Status, _ string) error {
	t.f.plans[planKey{planID, version}] = &planRow{spec: spec, status: status}
	return nil
}
func (t *fakeTx) PlanSpec(_ context.Context, planID uuid.UUID, version int) ([]byte, plan.Status, bool, error) {
	if version > 0 {
		r, ok := t.f.plans[planKey{planID, version}]
		if !ok {
			return nil, "", false, nil
		}
		return r.spec, r.status, true, nil
	}
	latest := 0
	for k := range t.f.plans {
		if k.id == planID && k.version > latest {
			latest = k.version
		}
	}
	if latest == 0 {
		return nil, "", false, nil
	}
	r := t.f.plans[planKey{planID, latest}]
	return r.spec, r.status, true, nil
}
func (t *fakeTx) LockPlan(ctx context.Context, planID uuid.UUID, version int) ([]byte, plan.Status, bool, error) {
	return t.PlanSpec(ctx, planID, version)
}
func (t *fakeTx) SetPlan(_ context.Context, planID uuid.UUID, version int, spec []byte, status plan.Status) error {
	r := t.f.plans[planKey{planID, version}]
	r.spec, r.status = spec, status
	return nil
}
func (t *fakeTx) SetSignedOffBy(_ context.Context, planID uuid.UUID, version int, by string) error {
	if t.f.failSignedOff != nil {
		return t.f.failSignedOff
	}
	t.f.plans[planKey{planID, version}].signedOffBy = by
	return nil
}
func (t *fakeTx) SetEnabledPins(_ context.Context, planID uuid.UUID, version, agentVersion int, model string) error {
	if t.f.failEnable != nil {
		return t.f.failEnable
	}
	r := t.f.plans[planKey{planID, version}]
	r.agentPin, r.modelPin = agentVersion, model
	return nil
}
func (t *fakeTx) RetirePlan(_ context.Context, planID uuid.UUID, version int) (bool, error) {
	r, ok := t.f.plans[planKey{planID, version}]
	if !ok || r.status != plan.StatusEnabled {
		return false, nil
	}
	r.status = plan.StatusRetired
	return true, nil
}
