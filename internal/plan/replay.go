package plan

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

type planStartedPayload struct {
	PlanID  string `json:"plan_id"`
	Version int    `json:"version"`
}

type stepEnteredPayload struct {
	StepID string `json:"step_id"`
}

// transitionPayload is what EventPlanTransition carries — Predicate is the
// JSON AST that fired (or nil for an unconditional edge), satisfying the
// demo's own bar: "the transition log names the predicate that fired."
type transitionPayload struct {
	From      string     `json:"from"`
	To        string     `json:"to"`
	Predicate *Predicate `json:"predicate,omitempty"`
}

type stepExitedPayload struct {
	StepID   string  `json:"step_id"`
	Bindings Context `json:"bindings,omitempty"`
}

type planCompletedPayload struct{}

// state is what replaying a plan session's own event log reconstructs
// (README task 8.5's own events — plan_started/step_entered/transition/
// step_exited/plan_completed — ARE the durable source of truth; nothing
// about a plan run lives anywhere else). Never a second source of truth:
// Executor.Resume calls this fresh every time rather than trusting any
// in-memory cache across a crash/restart.
type state struct {
	Vars        Context
	CurrentStep string // the step last entered with no matching exit yet; "" if none has ever entered or the plan already completed
	Iterations  map[string]int
	Completed   bool
}

func replayState(ctx context.Context, history []store.Event, decrypt func(context.Context, store.Event) ([]byte, error)) (state, error) {
	st := state{Vars: Context{}, Iterations: map[string]int{}}
	for _, e := range history {
		switch e.Type { //nolint:exhaustive // only the plan.* event types (plus the ones this loop deliberately ignores) are structurally relevant to reconstructing plan state
		case store.EventPlanStepEntered:
			plaintext, err := decrypt(ctx, e)
			if err != nil {
				return state{}, err
			}
			var p stepEnteredPayload
			if err := json.Unmarshal(plaintext, &p); err != nil {
				return state{}, fmt.Errorf("plan: unmarshal step_entered: %w", err)
			}
			st.CurrentStep = p.StepID
			st.Iterations[p.StepID]++
		case store.EventPlanStepExited:
			plaintext, err := decrypt(ctx, e)
			if err != nil {
				return state{}, err
			}
			var p stepExitedPayload
			if err := json.Unmarshal(plaintext, &p); err != nil {
				return state{}, fmt.Errorf("plan: unmarshal step_exited: %w", err)
			}
			for k, v := range p.Bindings {
				st.Vars[k] = v
			}
			st.CurrentStep = ""
		case store.EventPlanCompleted:
			st.Completed = true
			st.CurrentStep = ""
		}
	}
	return st, nil
}
