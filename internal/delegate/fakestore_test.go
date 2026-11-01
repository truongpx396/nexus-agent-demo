package delegate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// fakeStore is a delegate.Store over maps. Like a real transaction it snapshots
// on entry and restores on error, so a resolution that fails part-way leaves
// nothing behind.
type fakeStore struct {
	mu sync.Mutex

	sessions    map[uuid.UUID]eventlog.Session
	histories   map[uuid.UUID][]eventlog.Event
	delegations map[uuid.UUID]delegate.Delegation
	envelopes   map[uuid.UUID]*envelope
	events      []appended
	dek         crypto.DEK

	failAppend error
	failDraw   error
}

type envelope struct {
	ceiling, remaining cost.Money
	children           int
}

type appended struct {
	SessionID uuid.UUID
	Type      eventlog.EventType
	PairRef   *uuid.UUID
	Payload   any
}

func newFakeStore(t *testing.T) *fakeStore {
	t.Helper()
	dek, err := crypto.GenerateDEK("k1")
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	return &fakeStore{
		sessions: map[uuid.UUID]eventlog.Session{}, histories: map[uuid.UUID][]eventlog.Event{},
		delegations: map[uuid.UUID]delegate.Delegation{}, envelopes: map[uuid.UUID]*envelope{}, dek: dek,
	}
}

var _ delegate.Store = (*fakeStore)(nil)

func (f *fakeStore) InTenant(ctx context.Context, _ uuid.UUID, fn func(ctx context.Context, tx delegate.Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sessions, delegations, events := map[uuid.UUID]eventlog.Session{}, map[uuid.UUID]delegate.Delegation{}, append([]appended(nil), f.events...)
	envelopes := map[uuid.UUID]envelope{}
	for k, v := range f.sessions {
		sessions[k] = v
	}
	for k, v := range f.delegations {
		delegations[k] = v
	}
	for k, v := range f.envelopes {
		envelopes[k] = *v
	}
	if err := fn(ctx, &fakeTx{f: f}); err != nil {
		f.sessions, f.delegations, f.events = sessions, delegations, events
		f.envelopes = map[uuid.UUID]*envelope{}
		for k, v := range envelopes {
			v := v
			f.envelopes[k] = &v
		}
		return err
	}
	return nil
}

// seedChild creates a finished child session whose last content event holds
// output, and a pending delegation from a fresh parent to it.
func (f *fakeStore) seedChild(t *testing.T, status string, output string, d delegate.Delegation) (parent, child eventlog.Session, dg delegate.Delegation) {
	t.Helper()
	parent = eventlog.Session{SessionID: uuid.New(), Status: eventlog.SessionStatusSuspended}
	child = eventlog.Session{SessionID: uuid.New(), Status: status}
	f.sessions[parent.SessionID], f.sessions[child.SessionID] = parent, child

	plaintext, _ := json.Marshal(map[string]string{"body": output})
	sealed, err := crypto.Seal(f.dek, plaintext, tenant.String(), child.SessionID.String())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	f.histories[child.SessionID] = []eventlog.Event{{EventID: uuid.New(), SessionID: child.SessionID, Type: eventlog.EventContent, Payload: sealed, KeyID: f.dek.KeyID, SchemaVersion: eventlog.CurrentSchemaVersion}}

	d.DelegationID, d.TenantID, d.ParentSessionID, d.ChildSessionID, d.Status = uuid.New(), tenant, parent.SessionID, child.SessionID, delegate.StatusPending
	if d.ParentToolUseEventID == uuid.Nil && d.FanoutID == nil {
		d.ParentToolUseEventID = uuid.New()
	}
	f.delegations[d.DelegationID] = d
	return parent, child, d
}

func (f *fakeStore) eventTypes() []eventlog.EventType {
	var out []eventlog.EventType
	for _, e := range f.events {
		out = append(out, e.Type)
	}
	return out
}

type fakeTx struct{ f *fakeStore }

var _ delegate.Tx = (*fakeTx)(nil)

func (t *fakeTx) Append(_ context.Context, _, sessionID uuid.UUID, typ eventlog.EventType, _ *string, pairRef *uuid.UUID, payload any) (eventlog.Event, error) {
	if t.f.failAppend != nil {
		return eventlog.Event{}, t.f.failAppend
	}
	t.f.events = append(t.f.events, appended{SessionID: sessionID, Type: typ, PairRef: pairRef, Payload: payload})
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
func (t *fakeTx) SetSessionStatus(context.Context, uuid.UUID, string, *string) error { return nil }
func (t *fakeTx) ListEvents(_ context.Context, id uuid.UUID) ([]eventlog.Event, error) {
	return t.f.histories[id], nil
}
func (t *fakeTx) NewDEK(context.Context, uuid.UUID) (crypto.DEK, error) { return t.f.dek, nil }
func (t *fakeTx) Unwrap(_ context.Context, keyID string) (crypto.DEK, error) {
	if keyID != t.f.dek.KeyID {
		return crypto.DEK{}, fmt.Errorf("no key %s", keyID)
	}
	return t.f.dek, nil
}

func (t *fakeTx) InsertDelegation(_ context.Context, d delegate.Delegation) (delegate.Delegation, error) {
	d.DelegationID, d.Status, d.CreatedAt = uuid.New(), delegate.StatusPending, time.Now()
	t.f.delegations[d.DelegationID] = d
	return d, nil
}
func (t *fakeTx) GetDelegation(_ context.Context, id uuid.UUID) (delegate.Delegation, error) {
	d, ok := t.f.delegations[id]
	if !ok {
		return delegate.Delegation{}, fmt.Errorf("%w: delegation", delegate.ErrNotFound)
	}
	return d, nil
}
func (t *fakeTx) FindPendingByChild(_ context.Context, child uuid.UUID) (delegate.Delegation, bool, error) {
	for _, d := range t.f.delegations {
		if d.ChildSessionID == child && d.Status == delegate.StatusPending {
			return d, true, nil
		}
	}
	return delegate.Delegation{}, false, nil
}
func (t *fakeTx) ListFanout(_ context.Context, fanoutID uuid.UUID) ([]delegate.Delegation, error) {
	var out []delegate.Delegation
	for _, d := range t.f.delegations {
		if d.FanoutID != nil && *d.FanoutID == fanoutID {
			out = append(out, d)
		}
	}
	return out, nil
}
func (t *fakeTx) CountForRoot(_ context.Context, root uuid.UUID) (open, total int, err error) {
	for _, d := range t.f.delegations {
		if t.f.sessions[d.ParentSessionID].RootSessionID == root {
			total++
			if d.Status == delegate.StatusPending {
				open++
			}
		}
	}
	return open, total, nil
}
func (t *fakeTx) SetDelegationResult(_ context.Context, id uuid.UUID, status delegate.Status, result json.RawMessage, reason string) error {
	d := t.f.delegations[id]
	d.Status, d.Result, d.Reason = status, result, reason
	t.f.delegations[id] = d
	return nil
}
func (t *fakeTx) Resolvable(_ context.Context, child uuid.UUID) (bool, error) {
	for _, d := range t.f.delegations {
		if d.ChildSessionID == child {
			return d.FanoutID != nil || d.ParentToolUseEventID != uuid.Nil, nil
		}
	}
	return true, nil
}

func (t *fakeTx) DrawEnvelope(_ context.Context, id uuid.UUID, amount cost.Money) (bool, cost.Money, error) {
	if t.f.failDraw != nil {
		return false, cost.Money{}, t.f.failDraw
	}
	e, ok := t.f.envelopes[id]
	if !ok {
		return false, cost.Money{}, fmt.Errorf("envelope %s not found", id)
	}
	if e.remaining.Micros < amount.Micros {
		return false, e.remaining, nil
	}
	e.remaining.Micros -= amount.Micros
	return true, e.remaining, nil
}
func (t *fakeTx) ReturnToEnvelope(_ context.Context, id uuid.UUID, amount cost.Money) error {
	t.f.envelopes[id].remaining.Micros += amount.Micros
	return nil
}
func (t *fakeTx) EnvelopeShare(_ context.Context, id uuid.UUID) (cost.Money, int, error) {
	e, ok := t.f.envelopes[id]
	if !ok {
		return cost.Money{}, 0, errors.New("no such envelope")
	}
	return e.ceiling, e.children, nil
}
