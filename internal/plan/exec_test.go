package plan_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/plan"
	"github.com/truongpx396/nexus-agent-demo/internal/runstate"
)

// startPlan runs p to its end on a fresh session and returns the store. The
// plans here use only condition and loop steps, which the executor evaluates
// itself with no model call — so no Kernel is needed, which is the point of the
// zero-token routing rule (task 8.6).
func startPlan(t *testing.T, p plan.Plan) (*fakeStore, uuid.UUID) {
	t.Helper()
	f := newFakeStore()
	sid := uuid.New()
	f.sessions[sid] = eventlog.Session{SessionID: sid, Status: eventlog.SessionStatusQueued}
	dek, err := crypto.GenerateDEK("k")
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	exec := &plan.Executor{Store: f}
	if err := exec.Start(context.Background(), tenant, sid, p, runstate.Seal(dek, tenant, sid)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return f, sid
}

func count(f *fakeStore, typ eventlog.EventType) int {
	n := 0
	for _, e := range f.events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func TestStart_WalksATwoStepPlanToCompletionAndRecordsEveryMove(t *testing.T) {
	p := plan.Plan{
		PlanID: uuid.New(), Version: 1, StartStep: "a",
		Steps: []plan.Step{
			{ID: "a", Kind: plan.StepCondition, Condition: &plan.ConditionConfig{}, Transitions: []plan.Transition{{To: "b"}}},
			{ID: "b", Kind: plan.StepCondition, Condition: &plan.ConditionConfig{}},
		},
	}
	f, sid := startPlan(t, p)

	want := []eventlog.EventType{
		eventlog.EventPlanStarted,
		eventlog.EventPlanStepEntered, eventlog.EventPlanStepExited, eventlog.EventPlanTransition, // a
		eventlog.EventPlanStepEntered, eventlog.EventPlanStepExited, eventlog.EventPlanTransition, // b
		eventlog.EventPlanCompleted,
	}
	got := f.eventTypes()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %s, want %s (all: %v)", i, got[i], want[i], got)
		}
	}
	if !f.events[0].First {
		t.Error("plan_started was not appended as the session's first event: it has no earlier event to find a key from")
	}
	if s := f.sessions[sid]; s.Status != eventlog.SessionStatusCompleted || s.TerminalReason == nil || *s.TerminalReason != "plan_completed" {
		t.Errorf("session = %s reason %v, want completed with plan_completed", s.Status, s.TerminalReason)
	}
}

func TestStart_ALoopRunsExactlyAsManyTimesAsItsPredicateAllows(t *testing.T) {
	lt3 := &plan.Predicate{Op: plan.OpLt, Field: "n", Value: plan.NumberValue(3)}
	p := plan.Plan{
		PlanID: uuid.New(), Version: 1, StartStep: "again",
		Steps: []plan.Step{
			{ID: "again", Kind: plan.StepLoop, Loop: &plan.LoopConfig{MaxIterations: 10, CounterVar: "n"},
				Transitions: []plan.Transition{{To: "again", When: lt3}, {To: "done"}}},
			{ID: "done", Kind: plan.StepCondition, Condition: &plan.ConditionConfig{}},
		},
	}
	f, sid := startPlan(t, p)

	if got := count(f, eventlog.EventPlanStepEntered); got != 4 {
		t.Errorf("steps entered = %d, want 4 (the loop three times, then done)", got)
	}
	if f.sessions[sid].Status != eventlog.SessionStatusCompleted {
		t.Errorf("session = %s, want completed", f.sessions[sid].Status)
	}
}

func TestStart_ALoopPastItsRuntimeBoundFailsRatherThanSpinning(t *testing.T) {
	always := &plan.Predicate{Op: plan.OpGt, Field: "n", Value: plan.NumberValue(0)}
	p := plan.Plan{
		PlanID: uuid.New(), Version: 1, StartStep: "spin",
		Steps: []plan.Step{
			{ID: "spin", Kind: plan.StepLoop, Loop: &plan.LoopConfig{MaxIterations: 2, CounterVar: "n"},
				Transitions: []plan.Transition{{To: "spin", When: always}}},
		},
	}
	f := newFakeStore()
	sid := uuid.New()
	f.sessions[sid] = eventlog.Session{SessionID: sid}
	dek, _ := crypto.GenerateDEK("k")

	err := (&plan.Executor{Store: f}).Start(context.Background(), tenant, sid, p, runstate.Seal(dek, tenant, sid))
	if err == nil {
		t.Fatal("a loop that never exits ran to completion: the runtime bound is the backstop for what Validate cannot see")
	}
	if got := count(f, eventlog.EventPlanStepEntered); got != 2 {
		t.Errorf("steps entered = %d, want exactly the bound of 2", got)
	}
	if f.sessions[sid].Status == eventlog.SessionStatusCompleted {
		t.Error("an errored plan was marked completed")
	}
}

func TestStart_StepEventsStayWhereTheyAreIfALaterStepFails(t *testing.T) {
	// An unrecognised step kind fails when it is dispatched; what came before it
	// is a true record of what happened and stays.
	p := plan.Plan{
		PlanID: uuid.New(), Version: 1, StartStep: "a",
		Steps: []plan.Step{
			{ID: "a", Kind: plan.StepCondition, Condition: &plan.ConditionConfig{}, Transitions: []plan.Transition{{To: "b"}}},
			{ID: "b", Kind: plan.StepKind("mystery")},
		},
	}
	f := newFakeStore()
	sid := uuid.New()
	f.sessions[sid] = eventlog.Session{SessionID: sid}
	dek, _ := crypto.GenerateDEK("k")

	if err := (&plan.Executor{Store: f}).Start(context.Background(), tenant, sid, p, runstate.Seal(dek, tenant, sid)); err == nil {
		t.Fatal("Start succeeded though a step kind is unknown")
	}
	if count(f, eventlog.EventPlanStepExited) != 1 || count(f, eventlog.EventPlanCompleted) != 0 {
		t.Errorf("events = %v, want step a recorded and no plan_completed", f.eventTypes())
	}
}
