package plan

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// Executor evaluates a Plan against one driving session — the platform
// itself walking the transition graph (README task 8.5), never the model:
// Eval (predicate.go) and every step of the loop below that isn't
// dispatching an "agent" step run with zero Provider.Stream calls, which is
// exactly what the zero-token routing test (task 8.6) asserts by counting
// them against internal/provider/fake across a full plan run.
type Executor struct {
	deps
	Kernel      *kernel.Kernel
	Approvals   *oversight.Approvals
	Inputs      *oversight.Inputs
	Delegations *delegate.Delegations
	System      string
	Catalog     []provider.ToolSchema
	LoadedTools []string
	MaxTurns    int
}

// Start begins a fresh plan run against sessionID (already created by the
// caller, with PlanID/PlanVersion pinned — README task 8.4's own note on
// why: an in-flight run must keep executing the EXACT version it started
// on). seal is the DEK-bound SealFunc the session's own creator minted
// (mirroring internal/surfaces/rest's own handleCreateRun) — every OTHER
// appendEvent call this package makes resolves the active key by looking at
// the session's own most recent event (deps.activeKeyID, the same
// out-of-band idiom internal/oversight and internal/delegate use), which
// only works because SOME event already exists; Start's own first append is
// always that session's very first event, so it has no history to resolve a
// key from and needs seal directly, exactly like Kernel.Run's own opening
// append does via RunState.Seal. Returns once the plan reaches
// plan_completed OR suspends on a gate/agent-step approval/fanout — never
// blocks waiting for either.
func (e *Executor) Start(ctx context.Context, tenantID, sessionID uuid.UUID, p Plan, seal kernel.SealFunc) error {
	if err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := e.appendFirstEvent(ctx, tx, tenantID, sessionID, seal, store.EventPlanStarted, planStartedPayload{PlanID: p.PlanID.String(), Version: p.Version}); err != nil {
			return err
		}
		return store.UpdateSessionStatus(ctx, tx, sessionID, store.SessionStatusRunning, nil)
	}); err != nil {
		return err
	}
	return e.run(ctx, tenantID, sessionID, p, state{Vars: Context{}, Iterations: map[string]int{}}, p.StartStep)
}

// Resume re-enters a plan an AGENT step's own ordinary kernel run suspended
// on (an in-step tool approval, not a plan-level gate) — call this AFTER
// whatever resolved that approval (internal/oversight.Resumer) has already
// driven the sub-run back to its own terminal state.
func (e *Executor) Resume(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	p, st, err := e.load(ctx, tenantID, sessionID)
	if err != nil {
		return err
	}
	if st.CurrentStep == "" {
		return fmt.Errorf("plan: session %s has no in-progress step to resume", sessionID)
	}
	step, ok := p.StepByID(st.CurrentStep)
	if !ok {
		return fmt.Errorf("plan: session %s's in-progress step %q no longer exists in its own pinned plan version", sessionID, st.CurrentStep)
	}
	if step.Kind != StepAgent {
		return fmt.Errorf("plan: session %s's in-progress step %q is a %s step; use ResumeGate instead", sessionID, st.CurrentStep, step.Kind)
	}
	bindings, suspended, err := e.continueAgentStep(ctx, tenantID, sessionID, step)
	if err != nil || suspended {
		return err
	}
	return e.afterStep(ctx, tenantID, sessionID, p, st, step, bindings)
}

// ResumeGate delivers a human decision for a suspended approval_gate,
// preauth, or input_request step — the caller (cmd/nexusd's own approval/
// input decision handlers) already has the resolved value in hand and
// hands it straight to the executor, rather than this package trying to
// re-discover which of possibly several approvals/input-requests against
// this session was actually its own gate's (an agent step's own in-line
// tool approval can also land on this same session id — see load's own
// doc note on that conflation).
func (e *Executor) ResumeGate(ctx context.Context, tenantID, sessionID uuid.UUID, value Value) error {
	p, st, err := e.load(ctx, tenantID, sessionID)
	if err != nil {
		return err
	}
	if st.CurrentStep == "" {
		return fmt.Errorf("plan: session %s has no in-progress step to resume", sessionID)
	}
	step, ok := p.StepByID(st.CurrentStep)
	if !ok {
		return fmt.Errorf("plan: session %s's in-progress step %q no longer exists in its own pinned plan version", sessionID, st.CurrentStep)
	}
	resultVar := gateResultVar(step)
	bindings := Context{}
	if resultVar != "" {
		bindings[resultVar] = value
	}
	if err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.UpdateSessionStatus(ctx, tx, sessionID, store.SessionStatusRunning, nil)
	}); err != nil {
		return err
	}
	return e.afterStep(ctx, tenantID, sessionID, p, st, step, bindings)
}

// ResolveFanout is internal/delegate.FanoutResolver's own method — called
// whenever ANY child of fanoutID reaches terminal (internal/delegate.
// Delegations.OnChildTerminal). It is idempotent and level-triggered: if
// the cohort isn't fully resolved yet, this is a documented no-op: the NEXT
// child's own completion calls it again.
func (e *Executor) ResolveFanout(ctx context.Context, tenantID, fanoutID uuid.UUID) error {
	members, err := e.Delegations.ListOpenForFanout(ctx, tenantID, fanoutID)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return fmt.Errorf("plan: fanout %s has no delegations", fanoutID)
	}
	for _, m := range members {
		if m.Status == delegate.StatusPending {
			return nil // cohort not fully resolved yet
		}
	}
	sessionID := members[0].ParentSessionID
	p, st, err := e.load(ctx, tenantID, sessionID)
	if err != nil {
		return err
	}
	if st.CurrentStep == "" {
		return fmt.Errorf("plan: session %s has no in-progress step to resume for fanout %s", sessionID, fanoutID)
	}
	step, ok := p.StepByID(st.CurrentStep)
	if !ok || step.DelegateFanout == nil {
		return fmt.Errorf("plan: session %s's in-progress step %q is not a delegate_fanout step", sessionID, st.CurrentStep)
	}

	returned := 0
	for _, m := range members {
		if m.Status == delegate.StatusReturned {
			returned++
		}
	}
	bindings := Context{}
	if v := step.DelegateFanout.CompletionVar; v != "" {
		bindings[v] = NumberValue(float64(returned))
	}
	if err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.UpdateSessionStatus(ctx, tx, sessionID, store.SessionStatusRunning, nil)
	}); err != nil {
		return err
	}
	return e.afterStep(ctx, tenantID, sessionID, p, st, step, bindings)
}

func gateResultVar(s Step) string {
	switch s.Kind { //nolint:exhaustive // only the two gate-shaped kinds carry a ResultVar; every other kind falls to the default ""
	case StepApprovalGate:
		return s.ApprovalGate.ResultVar
	case StepInputRequest:
		return s.InputRequest.ResultVar
	default:
		return ""
	}
}

// load rehydrates a plan session's spec (from sessions.plan_id/plan_version
// — the EXACT version pinned at start, task 8.4) and replays its own event
// log into a state.
//
// One honest scope note: an "agent" step's OWN in-line tool approval and a
// plan-level approval_gate/preauth/input_request BOTH create their
// approvals/input_requests rows against this SAME session id (README §8's
// own "no second suspension mechanism" — they deliberately share one
// primitive). Resume/ResumeGate are split by STEP KIND specifically so a
// caller never has to disambiguate which mechanism produced a given
// approval id; each entry point trusts its caller to have driven the RIGHT
// one already.
func (e *Executor) load(ctx context.Context, tenantID, sessionID uuid.UUID) (Plan, state, error) {
	var sess store.Session
	var history []store.Event
	if err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		sess, err = store.GetSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		history, err = store.ListEvents(ctx, tx, sessionID)
		return err
	}); err != nil {
		return Plan{}, state{}, err
	}
	if sess.PlanID == nil || sess.PlanVersion == nil {
		return Plan{}, state{}, fmt.Errorf("plan: session %s is not a plan-driven session", sessionID)
	}
	lc := &Lifecycle{Store: e.Store}
	p, _, err := lc.Get(ctx, tenantID, *sess.PlanID, *sess.PlanVersion)
	if err != nil {
		return Plan{}, state{}, err
	}
	st, err := replayState(ctx, history, e.decryptWith(tenantID, sessionID))
	if err != nil {
		return Plan{}, state{}, err
	}
	return p, st, nil
}
