package plan

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// runAgentStep appends a fresh user message (mirroring Kernel.Run's own
// opening message) and runs an ordinary kernel turn sequence to ITS OWN
// terminal — this is the one step kind that spends tokens; every other
// kind in this file spends none.
func (e *Executor) runAgentStep(ctx context.Context, tenantID, sessionID uuid.UUID, step Step) (Context, bool, error) {
	if err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.appendEvent(ctx, tx, tenantID, sessionID, store.EventUserMessage, nil, nil, agentInputPayload{Body: step.Agent.Input})
		return err
	}); err != nil {
		return nil, false, err
	}
	return e.continueAgentStep(ctx, tenantID, sessionID, step)
}

type agentInputPayload struct {
	Body string `json:"body"`
}

// continueAgentStep drives the session's kernel run forward via Continue —
// used both by a fresh entry (runAgentStep, right after appending the input
// message) and by Resume (re-entering a step whose sub-run suspended on an
// ordinary tool approval last time). Detects a fresh suspend the same way
// and returns suspended=true without touching Context — Resume is what
// gets called again once that resolves.
func (e *Executor) continueAgentStep(ctx context.Context, tenantID, sessionID uuid.UUID, step Step) (Context, bool, error) {
	rs, sess, err := e.loadRunState(ctx, tenantID, sessionID)
	if err != nil {
		return nil, false, err
	}
	cfg := kernel.RunConfig{System: e.System, Catalog: e.Catalog, MaxTurns: e.MaxTurns, AutonomyLevel: sess.AutonomyLevel, ModelID: sess.RouteModelID}
	for _, err := range e.Kernel.Continue(ctx, rs, cfg) {
		if err != nil {
			return nil, false, err
		}
	}

	var status string
	if err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s, err := store.GetSession(ctx, tx, sessionID)
		status = s.Status
		return err
	}); err != nil {
		return nil, false, err
	}
	if status == store.SessionStatusSuspended {
		return nil, true, nil
	}

	text, err := e.lastContentText(ctx, tenantID, sessionID)
	if err != nil {
		return nil, false, err
	}
	bindings := Context{}
	if v := step.Agent.OutputVar; v != "" {
		bindings[v] = StringValue(text)
	}
	return bindings, false, nil
}

// runDelegateFanout reserves the cohort's whole envelope up front (README
// task 8.13 — ONE call, before the first child starts), spawns every
// child sharing one fanoutID, and suspends: ResolveFanout (exec.go) is
// what re-enters once the whole cohort has resolved.
func (e *Executor) runDelegateFanout(ctx context.Context, tenantID, sessionID uuid.UUID, step Step) (Context, bool, error) {
	cfg := step.DelegateFanout
	if e.Delegations == nil {
		return nil, false, fmt.Errorf("plan: delegations not wired")
	}
	perChild, _, err := parseCeiling(cfg.PerChildCeiling)
	if err != nil {
		return nil, false, err
	}
	ceiling := cost.Money{Micros: perChild.Micros * int64(cfg.ChildCount), Currency: perChild.Currency}

	var envelopeID uuid.UUID
	if err := e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		envelopeID, err = delegate.CreateEnvelope(ctx, tx, tenantID, sessionID, ceiling, cfg.ChildCount)
		return err
	}); err != nil {
		return nil, false, err
	}

	fanoutID := uuid.New()
	for i := 0; i < cfg.ChildCount; i++ {
		if _, err := e.Delegations.Spawn(ctx, delegate.SpawnRequest{
			TenantID: tenantID, ParentSessionID: sessionID,
			AgentID: cfg.AgentID, Task: cfg.Task, ScopeGrant: cfg.ScopeGrant,
			FanoutID: &fanoutID, EnvelopeID: &envelopeID,
		}); err != nil {
			return nil, false, fmt.Errorf("plan: spawn fanout child %d/%d: %w", i+1, cfg.ChildCount, err)
		}
	}

	if err := e.suspend(ctx, tenantID, sessionID); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

// runApprovalGate and runPreauth both reuse internal/oversight.Approvals
// directly (README task 8.10's "no second suspension mechanism") — a
// plan-level gate is the SAME approval transaction a tool-use-triggered
// suspend uses, anchored to this step's own EventPlanStepEntered instead of
// a tool_use event; ResumeGate (exec.go) is what a human decision resumes
// through.
func (e *Executor) runApprovalGate(ctx context.Context, tenantID, sessionID, anchorEventID uuid.UUID, step Step) (Context, bool, error) {
	if e.Approvals == nil {
		return nil, false, fmt.Errorf("plan: approvals not wired")
	}
	toolID := "plan/" + step.ID
	input, _ := json.Marshal(map[string]string{"question": step.ApprovalGate.Question})
	if _, err := e.Approvals.Create(ctx, oversight.CreateApprovalRequest{
		TenantID: tenantID, SessionID: sessionID, ToolUseEventID: anchorEventID,
		ToolID: toolID, AskKind: "once",
		Context: oversight.ContextPackage{ToolID: toolID, Input: input},
	}); err != nil {
		return nil, false, err
	}
	if err := e.suspend(ctx, tenantID, sessionID); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

// runPreauth requests sign-off on the step's whole ENUMERATED digest set in
// one approval (README task 8.7) — validate.go already refused any preauth
// step whose enumeration was empty or unbounded, so what an approver sees
// here is always a closed, finite list.
func (e *Executor) runPreauth(ctx context.Context, tenantID, sessionID, anchorEventID uuid.UUID, step Step) (Context, bool, error) {
	if e.Approvals == nil {
		return nil, false, fmt.Errorf("plan: approvals not wired")
	}
	toolID := "plan/" + step.ID
	input, _ := json.Marshal(step.Preauth.Entries)
	if _, err := e.Approvals.Create(ctx, oversight.CreateApprovalRequest{
		TenantID: tenantID, SessionID: sessionID, ToolUseEventID: anchorEventID,
		ToolID: toolID, AskKind: "once",
		Context: oversight.ContextPackage{ToolID: toolID, Input: input},
	}); err != nil {
		return nil, false, err
	}
	if err := e.suspend(ctx, tenantID, sessionID); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

// runInputRequest pulls a structured answer — zero authorization value
// (internal/oversight/input.go's own task 5.9 rule), reused verbatim here.
func (e *Executor) runInputRequest(ctx context.Context, tenantID, sessionID uuid.UUID, step Step) (Context, bool, error) {
	if e.Inputs == nil {
		return nil, false, fmt.Errorf("plan: inputs not wired")
	}
	if _, err := e.Inputs.RequestInput(ctx, oversight.RequestInputParams{
		TenantID: tenantID, SessionID: sessionID,
		Question: step.InputRequest.Question, Schema: step.InputRequest.Schema,
	}); err != nil {
		return nil, false, err
	}
	if err := e.suspend(ctx, tenantID, sessionID); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

func (e *Executor) suspend(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	return e.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.UpdateSessionStatus(ctx, tx, sessionID, store.SessionStatusSuspended, nil)
	})
}
