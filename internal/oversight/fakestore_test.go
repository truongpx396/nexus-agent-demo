package oversight_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
)

// fakeStore is an oversight.Store over maps. Like a real transaction it
// snapshots on entry and restores on error, so a decision that fails part-way
// leaves nothing behind — which is what lets these tests pin that a status and
// its event only ever change together.
type fakeStore struct {
	mu sync.Mutex

	approvals map[uuid.UUID]oversight.Approval
	order     []uuid.UUID // insertion order, for PendingApprovals
	inputs    map[uuid.UUID]oversight.InputRequest
	owners    map[uuid.UUID]uuid.UUID // session -> owning user
	events    []appended

	failAppend error // makes every Append fail
}

type appended struct {
	SessionID uuid.UUID
	Type      eventlog.EventType
	ToolID    *string
	PairRef   *uuid.UUID
	Payload   any
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		approvals: map[uuid.UUID]oversight.Approval{},
		inputs:    map[uuid.UUID]oversight.InputRequest{},
		owners:    map[uuid.UUID]uuid.UUID{},
	}
}

var _ oversight.Store = (*fakeStore)(nil)

type snapshot struct {
	approvals map[uuid.UUID]oversight.Approval
	order     []uuid.UUID
	inputs    map[uuid.UUID]oversight.InputRequest
	events    []appended
}

func (f *fakeStore) snap() snapshot {
	s := snapshot{
		approvals: map[uuid.UUID]oversight.Approval{}, inputs: map[uuid.UUID]oversight.InputRequest{},
		order: append([]uuid.UUID(nil), f.order...), events: append([]appended(nil), f.events...),
	}
	for k, v := range f.approvals {
		s.approvals[k] = v
	}
	for k, v := range f.inputs {
		s.inputs[k] = v
	}
	return s
}

func (f *fakeStore) restore(s snapshot) {
	f.approvals, f.inputs, f.order, f.events = s.approvals, s.inputs, s.order, s.events
}

func (f *fakeStore) InTenant(ctx context.Context, _ uuid.UUID, fn func(ctx context.Context, tx oversight.Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	before := f.snap()
	if err := fn(ctx, &fakeTx{f: f}); err != nil {
		f.restore(before)
		return err
	}
	return nil
}

// seedApproval stores a pending approval for sessionID and returns it.
func (f *fakeStore) seedApproval(sessionID uuid.UUID, expires time.Time) oversight.Approval {
	ap := oversight.Approval{
		ApprovalID: uuid.New(), SessionID: sessionID, ToolUseEventID: uuid.New(), ToolID: "platform/shell@v1",
		CanonicalDigest: []byte("original-digest"), Status: oversight.ApprovalPending, ExpiresAt: expires,
	}
	f.approvals[ap.ApprovalID] = ap
	f.order = append(f.order, ap.ApprovalID)
	return ap
}

func (f *fakeStore) eventTypes() []eventlog.EventType {
	out := make([]eventlog.EventType, len(f.events))
	for i, e := range f.events {
		out[i] = e.Type
	}
	return out
}

type fakeTx struct{ f *fakeStore }

var _ oversight.Tx = (*fakeTx)(nil)

func (t *fakeTx) Append(_ context.Context, _, sessionID uuid.UUID, typ eventlog.EventType, toolID *string, pairRef *uuid.UUID, payload any) (eventlog.Event, error) {
	if t.f.failAppend != nil {
		return eventlog.Event{}, t.f.failAppend
	}
	t.f.events = append(t.f.events, appended{sessionID, typ, toolID, pairRef, payload})
	return eventlog.Event{Type: typ, SessionID: sessionID}, nil
}

func notFound(what string) error { return fmt.Errorf("%w: %s", oversight.ErrNotFound, what) }

func (t *fakeTx) InsertApproval(_ context.Context, ap *oversight.Approval) error {
	ap.CreatedAt = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	t.f.approvals[ap.ApprovalID] = *ap
	t.f.order = append(t.f.order, ap.ApprovalID)
	return nil
}

func (t *fakeTx) GetApproval(_ context.Context, id uuid.UUID) (oversight.Approval, error) {
	ap, ok := t.f.approvals[id]
	if !ok {
		return oversight.Approval{}, notFound("approval")
	}
	return ap, nil
}

func (t *fakeTx) LockApproval(ctx context.Context, id uuid.UUID) (oversight.Approval, error) {
	return t.GetApproval(ctx, id)
}

func (t *fakeTx) PendingApprovals(context.Context) ([]oversight.Approval, error) {
	var out []oversight.Approval
	for _, id := range t.f.order {
		if ap := t.f.approvals[id]; ap.Status == oversight.ApprovalPending {
			out = append(out, ap)
		}
	}
	return out, nil
}

func (t *fakeTx) LockPendingApproval(_ context.Context, sessionID uuid.UUID) (oversight.Approval, bool, error) {
	for _, id := range t.f.order {
		if ap := t.f.approvals[id]; ap.SessionID == sessionID && ap.Status == oversight.ApprovalPending {
			return ap, true, nil
		}
	}
	return oversight.Approval{}, false, nil
}

func (t *fakeTx) SetApprovalStatus(_ context.Context, id uuid.UUID, status oversight.ApprovalStatus, decidedBy, reason string) error {
	ap, ok := t.f.approvals[id]
	if !ok {
		return notFound("approval")
	}
	ap.Status, ap.DecidedBy, ap.Reason = status, decidedBy, reason
	t.f.approvals[id] = ap
	return nil
}

func (t *fakeTx) SetApprovalModified(_ context.Context, id uuid.UUID, decidedBy string, grantedInput, newDigest []byte) error {
	ap, ok := t.f.approvals[id]
	if !ok {
		return notFound("approval")
	}
	ap.Status, ap.DecidedBy, ap.GrantedInput, ap.CanonicalDigest = oversight.ApprovalGrantedModified, decidedBy, grantedInput, newDigest
	t.f.approvals[id] = ap
	return nil
}

func (t *fakeTx) SessionOwner(_ context.Context, sessionID uuid.UUID) (uuid.UUID, error) {
	owner, ok := t.f.owners[sessionID]
	if !ok {
		return uuid.Nil, notFound("session")
	}
	return owner, nil
}

func (t *fakeTx) InsertInputRequest(_ context.Context, ir *oversight.InputRequest) error {
	ir.CreatedAt = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	t.f.inputs[ir.InputRequestID] = *ir
	return nil
}

func (t *fakeTx) LockInputRequest(_ context.Context, id uuid.UUID) (oversight.InputRequest, error) {
	ir, ok := t.f.inputs[id]
	if !ok {
		return oversight.InputRequest{}, notFound("input request")
	}
	return ir, nil
}

func (t *fakeTx) LockPendingInputRequests(_ context.Context, sessionID uuid.UUID) ([]oversight.InputRequest, error) {
	var out []oversight.InputRequest
	for _, ir := range t.f.inputs {
		if ir.SessionID == sessionID && ir.Status == oversight.InputPending {
			out = append(out, ir)
		}
	}
	return out, nil
}

func (t *fakeTx) SetInputAnswered(_ context.Context, id uuid.UUID, answer []byte, usedDefault bool) error {
	ir, ok := t.f.inputs[id]
	if !ok {
		return notFound("input request")
	}
	ir.Status, ir.Answer, ir.UsedDefault = oversight.InputAnswered, answer, usedDefault
	t.f.inputs[id] = ir
	return nil
}

func (t *fakeTx) SetInputStatus(_ context.Context, id uuid.UUID, status oversight.InputStatus, reason string) error {
	ir, ok := t.f.inputs[id]
	if !ok {
		return notFound("input request")
	}
	ir.Status, ir.Reason = status, reason
	t.f.inputs[id] = ir
	return nil
}
