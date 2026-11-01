package runctl_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/queue"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
)

// fakeStore is a runctl.Store over maps. Like a real transaction it snapshots
// on entry and restores on error, so an operation that fails part-way leaves
// nothing behind — which is what lets these tests pin that a session's status,
// a claim and the event that records them only ever change together.
type fakeStore struct {
	mu sync.Mutex

	sessions map[uuid.UUID]eventlog.Session
	claims   map[uuid.UUID]runctl.Claim
	events   []appended
	minted   int // data keys minted

	failAppend error // makes every Append fail
}

type appended struct {
	SessionID uuid.UUID
	Type      eventlog.EventType
	ToolID    *string
	Payload   any
}

func newFakeStore() *fakeStore {
	return &fakeStore{sessions: map[uuid.UUID]eventlog.Session{}, claims: map[uuid.UUID]runctl.Claim{}}
}

var _ runctl.Store = (*fakeStore)(nil)

type snapshot struct {
	sessions map[uuid.UUID]eventlog.Session
	claims   map[uuid.UUID]runctl.Claim
	events   []appended
	minted   int
}

func (f *fakeStore) snap() snapshot {
	s := snapshot{
		sessions: map[uuid.UUID]eventlog.Session{}, claims: map[uuid.UUID]runctl.Claim{},
		events: append([]appended(nil), f.events...), minted: f.minted,
	}
	for k, v := range f.sessions {
		s.sessions[k] = v
	}
	for k, v := range f.claims {
		s.claims[k] = v
	}
	return s
}

func (f *fakeStore) InTenant(ctx context.Context, _ uuid.UUID, fn func(ctx context.Context, tx runctl.Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	before := f.snap()
	if err := fn(ctx, &fakeTx{f: f}); err != nil {
		f.sessions, f.claims, f.events, f.minted = before.sessions, before.claims, before.events, before.minted
		return err
	}
	return nil
}

func (f *fakeStore) seedSession(status, autonomy string) eventlog.Session {
	s := eventlog.Session{SessionID: uuid.New(), Status: status, AutonomyLevel: autonomy}
	f.sessions[s.SessionID] = s
	return s
}

func (f *fakeStore) eventTypes() []eventlog.EventType {
	var out []eventlog.EventType
	for _, e := range f.events {
		out = append(out, e.Type)
	}
	return out
}

type fakeTx struct{ f *fakeStore }

var _ runctl.Tx = (*fakeTx)(nil)

func (t *fakeTx) Append(_ context.Context, _, sessionID uuid.UUID, typ eventlog.EventType, toolID *string, _ *uuid.UUID, payload any) (eventlog.Event, error) {
	if t.f.failAppend != nil {
		return eventlog.Event{}, t.f.failAppend
	}
	t.f.events = append(t.f.events, appended{SessionID: sessionID, Type: typ, ToolID: toolID, Payload: payload})
	return eventlog.Event{EventID: uuid.New(), SessionID: sessionID, Type: typ, Seq: int64(len(t.f.events))}, nil
}

var errNoSession = errors.New("session not found")

func (t *fakeTx) GetSession(_ context.Context, id uuid.UUID) (eventlog.Session, error) {
	s, ok := t.f.sessions[id]
	if !ok {
		return eventlog.Session{}, errNoSession
	}
	return s, nil
}

func (t *fakeTx) CreateSession(_ context.Context, s eventlog.Session) error {
	t.f.sessions[s.SessionID] = s
	return nil
}

func (t *fakeTx) SetSessionStatus(_ context.Context, id uuid.UUID, status string, reason *string) error {
	s, ok := t.f.sessions[id]
	if !ok {
		return errNoSession
	}
	s.Status, s.TerminalReason = status, reason
	t.f.sessions[id] = s
	return nil
}

func (t *fakeTx) ListEvents(context.Context, uuid.UUID) ([]eventlog.Event, error) { return nil, nil }

func (t *fakeTx) NewDEK(context.Context, uuid.UUID) (crypto.DEK, error) {
	t.f.minted++
	return crypto.GenerateDEK(fmt.Sprintf("k%d", t.f.minted))
}

func (t *fakeTx) Unwrap(context.Context, string) (crypto.DEK, error) { return crypto.GenerateDEK("k") }

func (t *fakeTx) OpenOrFindClaim(_ context.Context, tenantID, sessionID uuid.UUID, toolID string, digest []byte) (runctl.Claim, bool, error) {
	for _, c := range t.f.claims {
		if c.SessionID == sessionID && string(c.CanonicalDigest) == string(digest) {
			return c, false, nil
		}
	}
	c := runctl.Claim{ClaimID: uuid.New(), TenantID: tenantID, SessionID: sessionID, ToolID: toolID, CanonicalDigest: digest, Status: runctl.ClaimInFlight, CreatedAt: time.Now()}
	t.f.claims[c.ClaimID] = c
	return c, true, nil
}

func (t *fakeTx) ResolveClaim(_ context.Context, id uuid.UUID, status runctl.ClaimStatus, reason string) (runctl.Claim, error) {
	c, ok := t.f.claims[id]
	if !ok {
		return runctl.Claim{}, errors.New("claim not found")
	}
	c.Status, c.Reason = status, reason
	t.f.claims[id] = c
	return c, nil
}

func (t *fakeTx) ListInFlightClaims(_ context.Context, sessionID uuid.UUID) ([]runctl.Claim, error) {
	var out []runctl.Claim
	for _, c := range t.f.claims {
		if c.SessionID == sessionID && c.Status == runctl.ClaimInFlight {
			out = append(out, c)
		}
	}
	return out, nil
}

func (t *fakeTx) ClaimAwaitingInput(_ context.Context, id uuid.UUID) (bool, error) {
	s, ok := t.f.sessions[id]
	if !ok || s.Status != eventlog.SessionStatusAwaitingInput {
		return false, nil
	}
	s.Status = eventlog.SessionStatusRunning
	t.f.sessions[id] = s
	return true, nil
}

func (t *fakeTx) SetAutonomyLevel(_ context.Context, id uuid.UUID, level string) error {
	s, ok := t.f.sessions[id]
	if !ok {
		return errNoSession
	}
	s.AutonomyLevel = level
	t.f.sessions[id] = s
	return nil
}

// fakeQueue records enqueued jobs.
type fakeQueue struct{ jobs []queue.Job }

func (q *fakeQueue) Enqueue(_ context.Context, j queue.Job) (queue.Job, error) {
	q.jobs = append(q.jobs, j)
	return j, nil
}
func (q *fakeQueue) Lease(context.Context, string, time.Duration) (queue.Job, bool, error) {
	return queue.Job{}, false, nil
}
func (q *fakeQueue) Complete(context.Context, uuid.UUID) error { return nil }
func (q *fakeQueue) Fail(context.Context, uuid.UUID, string, bool, time.Time) error {
	return nil
}

var _ queue.Port = (*fakeQueue)(nil)
