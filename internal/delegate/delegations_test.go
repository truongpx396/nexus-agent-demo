package delegate_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

var tenant = uuid.New()

// fakeFanout records which fan-out cohort a child's completion asked about.
type fakeFanout struct{ resolved []uuid.UUID }

func (f *fakeFanout) ResolveFanout(_ context.Context, _, fanoutID uuid.UUID) error {
	f.resolved = append(f.resolved, fanoutID)
	return nil
}

// fanoutDelegations wires Delegations so a child's completion routes to the
// recorded FanoutResolver instead of resuming a live kernel run.
func fanoutDelegations(f *fakeStore) (*delegate.Delegations, *fakeFanout) {
	fan := &fakeFanout{}
	return delegate.NewDelegations(f).Wire(delegate.Config{Fanout: fan}), fan
}

func TestOnChildTerminal_IsANoOpForASessionThatIsNoDelegationsChild(t *testing.T) {
	f := newFakeStore(t)
	d, fan := fanoutDelegations(f)

	if err := d.OnChildTerminal(context.Background(), tenant, uuid.New()); err != nil {
		t.Fatalf("OnChildTerminal: %v", err)
	}
	if len(f.events) != 0 || len(fan.resolved) != 0 {
		t.Errorf("an ordinary session changed state: events %v, fanouts %v", f.eventTypes(), fan.resolved)
	}
}

func TestOnChildTerminal_WaitsWhileTheChildIsStillRunning(t *testing.T) {
	f := newFakeStore(t)
	fanout := uuid.New()
	_, child, dg := f.seedChild(t, eventlog.SessionStatusSuspended, "partial", delegate.Delegation{FanoutID: &fanout})
	d, fan := fanoutDelegations(f)

	if err := d.OnChildTerminal(context.Background(), tenant, child.SessionID); err != nil {
		t.Fatalf("OnChildTerminal: %v", err)
	}
	if f.delegations[dg.DelegationID].Status != delegate.StatusPending || len(f.events) != 0 || len(fan.resolved) != 0 {
		t.Errorf("a child that has not finished resolved its delegation: %s, events %v", f.delegations[dg.DelegationID].Status, f.eventTypes())
	}
}

func TestOnChildTerminal_RecordsTheReturnOnTheParentAndAsksTheFanout(t *testing.T) {
	f := newFakeStore(t)
	fanout := uuid.New()
	parent, child, dg := f.seedChild(t, eventlog.SessionStatusCompleted, "the answer is 42", delegate.Delegation{FanoutID: &fanout, AgentID: "researcher"})
	d, fan := fanoutDelegations(f)

	if err := d.OnChildTerminal(context.Background(), tenant, child.SessionID); err != nil {
		t.Fatalf("OnChildTerminal: %v", err)
	}
	got := f.delegations[dg.DelegationID]
	if got.Status != delegate.StatusReturned || !strings.Contains(string(got.Result), "the answer is 42") {
		t.Errorf("delegation = %s result %s, want returned with the child's output", got.Status, got.Result)
	}
	// The parent's log records both the taint the child carried back and the
	// return itself, in the same unit as the status change.
	types := f.eventTypes()
	if len(types) != 2 || types[0] != eventlog.EventTaintTransition || types[1] != eventlog.EventDelegationReturned {
		t.Fatalf("events = %v, want taint_transition then delegation_returned", types)
	}
	for _, e := range f.events {
		if e.SessionID != parent.SessionID {
			t.Errorf("event %s appended to %s, want the parent %s", e.Type, e.SessionID, parent.SessionID)
		}
	}
	if len(fan.resolved) != 1 || fan.resolved[0] != fanout {
		t.Errorf("fanout asked about %v, want the child's cohort %s", fan.resolved, fanout)
	}
}

func TestOnChildTerminal_AFailedChildReturnsItsReasonAndNoResult(t *testing.T) {
	f := newFakeStore(t)
	fanout := uuid.New()
	_, child, dg := f.seedChild(t, eventlog.SessionStatusFailed, "ignored", delegate.Delegation{FanoutID: &fanout})
	reason := "max_turns"
	child.TerminalReason = &reason
	f.sessions[child.SessionID] = child
	d, _ := fanoutDelegations(f)

	if err := d.OnChildTerminal(context.Background(), tenant, child.SessionID); err != nil {
		t.Fatalf("OnChildTerminal: %v", err)
	}
	got := f.delegations[dg.DelegationID]
	if got.Result != nil || !strings.Contains(got.Reason, "child session failed: max_turns") {
		t.Errorf("delegation result %s reason %q, want no result and the child's terminal reason", got.Result, got.Reason)
	}
}

func TestOnChildTerminal_AReturnThatBreaksItsSchemaIsBoundExceeded(t *testing.T) {
	schema := json.RawMessage(`{"required":["summary","score"]}`)
	cases := map[string]string{
		"not a JSON object":      "just prose",
		"missing a required key": `{"summary":"ok"}`,
	}
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeStore(t)
			fanout := uuid.New()
			_, child, dg := f.seedChild(t, eventlog.SessionStatusCompleted, output, delegate.Delegation{FanoutID: &fanout, ReturnSchema: schema})
			d, _ := fanoutDelegations(f)

			if err := d.OnChildTerminal(context.Background(), tenant, child.SessionID); err != nil {
				t.Fatalf("OnChildTerminal: %v", err)
			}
			got := f.delegations[dg.DelegationID]
			if got.Status != delegate.StatusBoundExceeded || got.Result != nil || got.Reason == "" {
				t.Errorf("delegation = %s result %s reason %q, want bound_exceeded with a reason and no result", got.Status, got.Result, got.Reason)
			}
		})
	}

	t.Run("a conforming return is accepted verbatim", func(t *testing.T) {
		f := newFakeStore(t)
		fanout := uuid.New()
		_, child, dg := f.seedChild(t, eventlog.SessionStatusCompleted, `{"summary":"ok","score":3}`, delegate.Delegation{FanoutID: &fanout, ReturnSchema: schema})
		d, _ := fanoutDelegations(f)
		if err := d.OnChildTerminal(context.Background(), tenant, child.SessionID); err != nil {
			t.Fatalf("OnChildTerminal: %v", err)
		}
		if got := f.delegations[dg.DelegationID]; got.Status != delegate.StatusReturned || string(got.Result) != `{"summary":"ok","score":3}` {
			t.Errorf("delegation = %s result %s, want returned with the object as is", got.Status, got.Result)
		}
	})
}

func TestOnChildTerminal_ResolutionIsAllOrNothing(t *testing.T) {
	f := newFakeStore(t)
	fanout := uuid.New()
	_, child, dg := f.seedChild(t, eventlog.SessionStatusCompleted, "done", delegate.Delegation{FanoutID: &fanout})
	f.failAppend = errors.New("audit chain unavailable")
	d, fan := fanoutDelegations(f)

	if err := d.OnChildTerminal(context.Background(), tenant, child.SessionID); err == nil {
		t.Fatal("OnChildTerminal succeeded though the parent's events could not be recorded")
	}
	if f.delegations[dg.DelegationID].Status != delegate.StatusPending || len(fan.resolved) != 0 {
		t.Errorf("delegation = %s, fanouts %v, want it still pending: a status must never change without its events", f.delegations[dg.DelegationID].Status, fan.resolved)
	}
}

func TestOnChildTerminal_AFanoutChildWithNoResolverIsAnError(t *testing.T) {
	f := newFakeStore(t)
	fanout := uuid.New()
	_, child, _ := f.seedChild(t, eventlog.SessionStatusCompleted, "done", delegate.Delegation{FanoutID: &fanout})

	if err := delegate.NewDelegations(f).OnChildTerminal(context.Background(), tenant, child.SessionID); err == nil {
		t.Fatal("a fan-out child resolved with nothing wired to ask whether its cohort is done")
	}
}

func TestParentContextAndCountForRoot(t *testing.T) {
	f := newFakeStore(t)
	root := uuid.New()
	parent := eventlog.Session{SessionID: uuid.New(), Depth: 1, RootSessionID: root}
	f.sessions[parent.SessionID] = parent
	pending := delegate.Delegation{ParentSessionID: parent.SessionID, Status: delegate.StatusPending}
	returned := delegate.Delegation{ParentSessionID: parent.SessionID, Status: delegate.StatusReturned}
	f.delegations[uuid.New()], f.delegations[uuid.New()] = pending, returned
	d := delegate.NewDelegations(f)

	depth, gotRoot, err := d.ParentContext(context.Background(), tenant, parent.SessionID)
	if err != nil || depth != 1 || gotRoot != root {
		t.Errorf("ParentContext = %d, %s, %v, want depth 1 under %s", depth, gotRoot, err, root)
	}
	open, total, err := d.CountForRoot(context.Background(), tenant, root)
	if err != nil || open != 1 || total != 2 {
		t.Errorf("CountForRoot = open %d total %d, %v, want 1 open of 2: the concurrent and per-run bounds count different things", open, total, err)
	}
}

func envelopeGate(f *fakeStore, ceiling, remaining, perCall int64, prices delegate.PriceBooks) (*delegate.EnvelopeBudgetGate, uuid.UUID) {
	id := uuid.New()
	f.envelopes[id] = &envelope{ceiling: cost.Money{Micros: ceiling, Currency: "USD"}, remaining: cost.Money{Micros: remaining, Currency: "USD"}, children: 4}
	return &delegate.EnvelopeBudgetGate{Store: f, Prices: prices, EnvelopeID: id, TenantID: tenant, PerCallEstimate: cost.Money{Micros: perCall, Currency: "USD"}, Currency: "USD"}, id
}

func TestEnvelopeBudgetGate_DrawsFromTheSharedEnvelopeAndRefusesOnceItIsSpent(t *testing.T) {
	f := newFakeStore(t)
	g, id := envelopeGate(f, 2000, 2000, 1000, nil)

	for i := 0; i < 2; i++ {
		res, err := g.Reserve(context.Background(), cost.ReserveRequest{TenantID: tenant, SessionID: uuid.New()})
		if err != nil || res.Decision.Kind != cost.DecisionAllow {
			t.Fatalf("Reserve %d = %v, %v, want allowed while the envelope has room", i+1, res.Decision.Kind, err)
		}
	}
	res, err := g.Reserve(context.Background(), cost.ReserveRequest{TenantID: tenant, SessionID: uuid.New()})
	if err == nil || res.Decision.Kind != cost.DecisionRefuseCeiling {
		t.Fatalf("third Reserve = %v, %v, want refused: the cohort's ceiling is shared, not per child", res.Decision.Kind, err)
	}
	if f.envelopes[id].remaining.Micros != 0 {
		t.Errorf("remaining = %d, want 0 and never negative", f.envelopes[id].remaining.Micros)
	}
}

func TestEnvelopeBudgetGate_FailsClosedWhenTheEnvelopeCannotBeRead(t *testing.T) {
	f := newFakeStore(t)
	g, _ := envelopeGate(f, 1000, 1000, 100, nil)
	f.failDraw = errors.New("database unavailable")

	res, err := g.Reserve(context.Background(), cost.ReserveRequest{TenantID: tenant, SessionID: uuid.New()})
	if err == nil || res.Decision.Kind != cost.DecisionRefuseCeiling {
		t.Errorf("Reserve with an unreadable envelope = %v, %v, want refused, never allowed unmetered", res.Decision.Kind, err)
	}
}

type fixedPrices struct{ pb *cost.PriceBook }

func (p fixedPrices) LoadPriceBook(context.Context, uuid.UUID) (*cost.PriceBook, error) {
	return p.pb, nil
}

func TestEnvelopeBudgetGate_ReconcileGivesBackWhatTheCallDidNotUse(t *testing.T) {
	f := newFakeStore(t)
	// 1,000,000 micros per million output tokens = one micro per token.
	pb := cost.NewPriceBook([]cost.PriceBookEntry{{
		ID: uuid.New(), Meter: cost.MeterOutput, Subject: cost.WildcardSubject, Version: 1, Currency: "USD",
		PricePerMillionMicros: 1_000_000, EffectiveFrom: time.Now().Add(-time.Hour),
	}})
	g, id := envelopeGate(f, 5000, 5000, 1000, fixedPrices{pb})

	res, err := g.Reserve(context.Background(), cost.ReserveRequest{TenantID: tenant, SessionID: uuid.New(), ModelID: "m"})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := g.Reconcile(context.Background(), res, provider.Usage{OutputTokens: 250}, true); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// 5000 - 1000 reserved + (1000 - 250 actual) returned = 4750.
	if got := f.envelopes[id].remaining.Micros; got != 4750 {
		t.Errorf("remaining = %d, want 4750: the unused 750 goes back for the other children", got)
	}
}

func TestEnvelopeBudgetGate_AnUnreportedCallKeepsItsWholeReservation(t *testing.T) {
	f := newFakeStore(t)
	g, id := envelopeGate(f, 5000, 5000, 1000, nil)
	res, _ := g.Reserve(context.Background(), cost.ReserveRequest{TenantID: tenant, SessionID: uuid.New()})

	if err := g.Reconcile(context.Background(), res, provider.Usage{}, false); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := f.envelopes[id].remaining.Micros; got != 4000 {
		t.Errorf("remaining = %d, want 4000: usage that was never reported is charged at the worst case", got)
	}
}
