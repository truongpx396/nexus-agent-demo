package plan

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// run is the platform's own zero-token transition loop (README task 8.5):
// enter a step, dispatch it, exit it, evaluate its transitions, and repeat
// — stopping either because the plan completed or because a step
// suspended (Start/Resume/ResumeGate/ResolveFanout's own doc comments cover
// how execution picks back up from there).
func (e *Executor) run(ctx context.Context, tenantID, sessionID uuid.UUID, p Plan, st state, stepID string) error {
	for stepID != "" {
		step, ok := p.StepByID(stepID)
		if !ok {
			return fmt.Errorf("plan: step %q not found in plan %s version %d", stepID, p.PlanID, p.Version)
		}
		// Runtime backstop for task 8.3's bounded-loop validation: even a
		// validated plan is re-checked here, since Iterations is this RUN's
		// own count, not a static property Validate could see in advance.
		if step.Kind == StepLoop && st.Iterations[step.ID] >= step.Loop.MaxIterations {
			return fmt.Errorf("plan: step %q exceeded its max_iterations bound (%d) at runtime", step.ID, step.Loop.MaxIterations)
		}

		entered, err := e.enterStep(ctx, tenantID, sessionID, step)
		if err != nil {
			return err
		}
		st.CurrentStep = step.ID
		st.Iterations[step.ID]++

		bindings, suspended, err := e.dispatch(ctx, tenantID, sessionID, entered.EventID, step, st.Iterations[step.ID])
		if err != nil {
			return err
		}
		if suspended {
			return nil
		}

		next, err := e.exitStepAndTransition(ctx, tenantID, sessionID, &st, step, bindings)
		if err != nil {
			return err
		}
		stepID = next
	}
	return nil
}

// afterStep is what every suspend-resolution entry point (Resume,
// ResumeGate, ResolveFanout) shares: finish the step that was in progress
// with its now-known bindings, evaluate its transitions, and continue the
// SAME loop run drives — a resumed plan is not a special case past this
// point, just a run() call that starts mid-graph instead of at StartStep.
func (e *Executor) afterStep(ctx context.Context, tenantID, sessionID uuid.UUID, p Plan, st state, step Step, bindings Context) error {
	next, err := e.exitStepAndTransition(ctx, tenantID, sessionID, &st, step, bindings)
	if err != nil {
		return err
	}
	if next == "" {
		return nil
	}
	return e.run(ctx, tenantID, sessionID, p, st, next)
}

func (e *Executor) enterStep(ctx context.Context, tenantID, sessionID uuid.UUID, step Step) (store.Event, error) {
	var ev store.Event
	err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ev, err = e.appendEvent(ctx, tx, tenantID, sessionID, store.EventPlanStepEntered, nil, nil, stepEnteredPayload{StepID: step.ID})
		return err
	})
	return ev, err
}

// exitStepAndTransition appends step_exited (folding bindings into st.Vars
// as it does), evaluates the step's own Transitions at ZERO token cost
// (predicate.go's Eval — task 8.6), and appends the transition event naming
// whichever predicate fired (the demo's own "transition log names the
// predicate that fired" bar). An unmatched set of transitions (no default
// edge, none whose predicate held) is exactly how a plan run legitimately
// ends: appends plan_completed and marks the session completed.
func (e *Executor) exitStepAndTransition(ctx context.Context, tenantID, sessionID uuid.UUID, st *state, step Step, bindings Context) (string, error) {
	if err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.appendEvent(ctx, tx, tenantID, sessionID, store.EventPlanStepExited, nil, nil, stepExitedPayload{StepID: step.ID, Bindings: bindings})
		return err
	}); err != nil {
		return "", err
	}
	for k, v := range bindings {
		st.Vars[k] = v
	}

	next, pred, err := evaluateTransitions(step.Transitions, st.Vars)
	if err != nil {
		return "", err
	}

	if err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.appendEvent(ctx, tx, tenantID, sessionID, store.EventPlanTransition, nil, nil, transitionPayload{From: step.ID, To: next, Predicate: pred})
		return err
	}); err != nil {
		return "", err
	}

	if next == "" {
		if err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := e.appendEvent(ctx, tx, tenantID, sessionID, store.EventPlanCompleted, nil, nil, planCompletedPayload{}); err != nil {
				return err
			}
			reason := "plan_completed"
			return store.UpdateSessionStatus(ctx, tx, sessionID, store.SessionStatusCompleted, &reason)
		}); err != nil {
			return "", err
		}
	}
	return next, nil
}

// evaluateTransitions walks s in order, returning the first whose predicate
// holds (or the first with a nil When, the default edge — validate.go's own
// check already guarantees at most one exists, listed last). No match
// (next=="") is the plan's own natural end, not an error.
func evaluateTransitions(transitions []Transition, vars Context) (next string, fired *Predicate, err error) {
	for _, t := range transitions {
		if t.When == nil {
			return t.To, nil, nil
		}
		ok, err := t.When.Eval(vars)
		if err != nil {
			return "", nil, err
		}
		if ok {
			return t.To, t.When, nil
		}
	}
	return "", nil, nil
}

func (e *Executor) dispatch(ctx context.Context, tenantID, sessionID, anchorEventID uuid.UUID, step Step, iteration int) (Context, bool, error) {
	switch step.Kind {
	case StepAgent:
		return e.runAgentStep(ctx, tenantID, sessionID, step)
	case StepCondition:
		return nil, false, nil
	case StepLoop:
		bindings := Context{}
		if v := step.Loop.CounterVar; v != "" {
			bindings[v] = NumberValue(float64(iteration))
		}
		return bindings, false, nil
	case StepDelegateFanout:
		return e.runDelegateFanout(ctx, tenantID, sessionID, step)
	case StepApprovalGate:
		return e.runApprovalGate(ctx, tenantID, sessionID, anchorEventID, step)
	case StepPreauth:
		return e.runPreauth(ctx, tenantID, sessionID, anchorEventID, step)
	case StepInputRequest:
		return e.runInputRequest(ctx, tenantID, sessionID, step)
	default:
		return nil, false, fmt.Errorf("plan: unrecognized step kind %q", step.Kind)
	}
}
