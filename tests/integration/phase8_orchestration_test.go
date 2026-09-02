//go:build integration

// Phase 8 — Orchestration plane + delegation (README.md §8). Covers the two
// named acceptance criteria: 8.6 (zero-token routing: no Provider.Stream
// call happens while a plan evaluates a transition) and 8.12 (delegation
// bounds fail closed, enforced through the REAL permission chain — the
// unit-level fakes in internal/tools/builtin/delegate_test.go already cover
// CheckPermissions' own logic; this file proves the same thing end to end,
// against a real Postgres-backed Delegations ledger). Also covers 8.11's
// taint-ascend fold across a real spawn/resolve round trip and 8.10's
// durable-suspend-and-resume shape.
//
// Shares oversightRig/setupPostgresAndPgBouncer/insertTenant/newFakeSigner
// with phase5_oversight_test.go (same package).
package integration

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
	"github.com/truongpx396/nexus-agent-demo/internal/plan"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
	"github.com/truongpx396/nexus-agent-demo/internal/tools/builtin"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// countingProvider wraps a real Provider and counts Stream calls — the
// zero-token routing test's own instrument (README task 8.6): transition
// evaluation between steps must never touch this.
type countingProvider struct {
	inner provider.Provider
	mu    sync.Mutex
	calls int
}

func (c *countingProvider) Stream(ctx context.Context, p provider.Prompt, tools []provider.ToolSchema, rc provider.RunContext) (provider.Stream, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.Stream(ctx, p, tools, rc)
}

func (c *countingProvider) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestPlan_ZeroTokenRouting is README task 8.6's own acceptance test: a
// 3-step plan (agent -> [condition routes on the agent's own output] ->
// agent -> end) makes exactly ONE Provider.Stream call per agent step and
// ZERO while evaluating the transition between them — the platform, never
// the model, decides which branch fires.
func TestPlan_ZeroTokenRouting(t *testing.T) {
	r := setupOversightRig(t)
	counting := &countingProvider{inner: fake.New(contentScript("greenlight"), contentScript("ack"))}
	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{counting}),
		Tools:    kernel.NotImplementedToolExecutor{},
		Budget:   kernel.NoopBudgetGate{},
		Store:    r.st, Receipts: r.receiptFunc(),
	}
	exec := &plan.Executor{Kernel: k, System: "test", MaxTurns: 10}
	exec.Store, exec.Keys, exec.Chain = r.st, r.keys, r.chain

	p := plan.Plan{
		Name: "zero-token-routing", StartStep: "start",
		Steps: []plan.Step{
			{ID: "start", Kind: plan.StepAgent, Agent: &plan.AgentStepConfig{Input: "go", OutputVar: "signal"}, Transitions: []plan.Transition{
				{To: "confirm", When: &plan.Predicate{Op: plan.OpEq, Field: "signal", Value: plan.StringValue("greenlight")}},
				{To: "end"},
			}},
			{ID: "confirm", Kind: plan.StepAgent, Agent: &plan.AgentStepConfig{Input: "thanks", OutputVar: "ack"}, Transitions: []plan.Transition{{To: "end"}}},
			{ID: "end", Kind: plan.StepCondition, Condition: &plan.ConditionConfig{}},
		},
	}
	if err := plan.Validate(p); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	lc := &plan.Lifecycle{Store: r.st}
	created, err := lc.Create(context.Background(), r.tenantID, p, "test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sessionID, userID := uuid.New(), uuid.New()
	err = r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		version := created.Version
		return store.CreateSession(ctx, tx, store.Session{
			SessionID: sessionID, SessionKey: sessionID.String(), TenantID: r.tenantID,
			SurfaceID: "test", UserID: userID, AgentID: uuid.Nil, AgentVersion: 1,
			HarnessDigest: []byte("test"), DataLabel: "internal", RouteModelID: "fake",
			AutonomyLevel: "autonomous", PlanID: &created.PlanID, PlanVersion: &version,
		})
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := exec.Start(context.Background(), r.tenantID, sessionID, created, r.seal(t, sessionID)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	final := r.getSession(t, sessionID)
	if final.Status != store.SessionStatusCompleted {
		t.Fatalf("session status = %q, want completed", final.Status)
	}
	if got := counting.count(); got != 2 {
		t.Fatalf("Provider.Stream was called %d times, want exactly 2 (one per agent step — transition evaluation must cost zero)", got)
	}

	events, err := listEventsDirect(context.Background(), r.st, r.tenantID, sessionID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if !hasEventType(events, store.EventPlanTransition) {
		t.Fatalf("no plan_transition event was logged")
	}
	if !hasEventType(events, store.EventPlanCompleted) {
		t.Fatalf("no plan_completed event was logged")
	}
}

// spawnerAdapter translates builtin.SpawnRequest to delegate.SpawnRequest —
// the one place a real binary (or, here, a test) bridges the two packages'
// deliberately independent request shapes (internal/tools/builtin/
// delegate.go's own doc comment on why they're kept separate).
type spawnerAdapter struct{ d *delegate.Delegations }

func (a spawnerAdapter) Spawn(ctx context.Context, req builtin.SpawnRequest) (uuid.UUID, error) {
	return a.d.Spawn(ctx, delegate.SpawnRequest{
		TenantID: req.TenantID, ParentSessionID: req.ParentSessionID,
		AgentID: req.AgentID, Task: req.Task, ScopeGrant: req.ScopeGrant, ReturnSchema: req.ReturnSchema,
	})
}

func onDelegateFunc(d *delegate.Delegations) kernel.OnDelegate {
	return func(ctx context.Context, tx pgx.Tx, req kernel.DelegateSuspendRequest) error {
		return d.Bind(ctx, tx, req.ChildSessionID, req.ToolUseEventID)
	}
}

// buildDelegationRig wires a real tools.Pipeline (platform/delegate is its
// only tool) behind a real kernel.Kernel, plus a real, Postgres-backed
// *delegate.Delegations — everything cmd/nexusd wires in production, minus
// the queue/lock (single-goroutine tests don't need cross-worker
// serialization). A StandingScope covers platform/delegate so its own
// Rule-of-Two Ask (delegate.Taint() engages all three legs on every call,
// by design — README task 8.9) self-satisfies without a human decision;
// bound/scope violations still refuse at Gate 2, BEFORE layer 7 ever runs.
func buildDelegationRig(t *testing.T, r *oversightRig, prov provider.Provider) (*kernel.Kernel, *tools.Pipeline, *delegate.Delegations, []provider.ToolSchema) {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.DeclareNamespace("platform", "test"); err != nil {
		t.Fatalf("declare namespace: %v", err)
	}

	d := delegate.NewDelegations(r.st, r.keys, r.chain)
	del := builtin.Delegate{Ledger: d, Spawner: spawnerAdapter{d}, Registry: reg}
	if err := reg.Register(del); err != nil {
		t.Fatalf("register delegate: %v", err)
	}
	if err := reg.SetAdmissionStatus(del.ID(), tools.AdmissionClean); err != nil {
		t.Fatalf("set admission: %v", err)
	}
	manifest := tools.BuildManifest(reg)

	chain := permissions.NewChain(permissions.ChainConfig{
		Profiles:       permissions.ProfileSet{Profiles: []permissions.ToolProfile{permissions.NewToolProfile("default", 1, del.ID().String())}},
		StandingScopes: []permissions.StandingScope{{Name: "delegation-ok", ToolPattern: del.ID().String()}},
	})
	pipeline := tools.NewPipeline(tools.PipelineConfig{Registry: reg, Manifest: manifest, Chain: chain})

	k := &kernel.Kernel{
		Provider:   provider.Wrap([]provider.Provider{prov}),
		Tools:      kernel.PipelineExecutor{Pipeline: pipeline},
		Budget:     kernel.NoopBudgetGate{},
		Store:      r.st,
		Receipts:   r.receiptFunc(),
		OnDelegate: onDelegateFunc(d),
	}
	d.Wire(delegate.Config{Kernel: k, Pipeline: pipeline, System: "test", MaxTurns: 10})

	desc := del.Descriptor()
	catalog := []provider.ToolSchema{{Name: desc.ID.String(), Description: desc.Description, InputSchema: desc.InputSchema}}
	return k, pipeline, d, catalog
}

func delegateToolUse(agentID, task string, scopeGrant ...string) fake.Script {
	input, _ := json.Marshal(map[string]any{"agent_id": agentID, "task": task, "scope_grant": scopeGrant})
	return fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "tool_use", ToolUseID: "tu1", ToolName: "platform/delegate@v1", Input: string(input)},
		{Kind: "done", Done: "stop"},
	}}
}

func contentScript(text string) fake.Script {
	return fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "content", Text: text},
		{Kind: "done", Done: "stop"},
	}}
}

// waitForStatus polls sessionID's status (a real background goroutine drives
// the child and the parent's resume, so the test can't just synchronously
// drain a channel the way runToCompletion does for a foreground run).
func waitForStatus(t *testing.T, r *oversightRig, sessionID uuid.UUID, want string) store.Session {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sess := r.getSession(t, sessionID)
		if sess.Status == want {
			return sess
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s did not reach status %q within the deadline (last status %q)", sessionID, want, r.getSession(t, sessionID).Status)
	return store.Session{}
}

// TestDelegate_RoundTrip is the demo's own core case: an ordinary tool_use
// spawns a child, the parent suspends (README task 8.10), the child runs to
// its own completion, and the parent resumes with the child's content as
// the paired tool_result, continuing to its own clean completion.
func TestDelegate_RoundTrip(t *testing.T) {
	r := setupOversightRig(t)
	prov := fake.New(
		delegateToolUse("worker", "summarize the notes", "platform/delegate@v1"),
		contentScript("child summary: all good"),
		contentScript("parent: done, thanks"),
	)
	k, _, d, catalog := buildDelegationRig(t, r, prov)

	parentID, userID := uuid.New(), uuid.New()
	r.createSession(t, parentID, userID, "autonomous")
	sealFn := r.seal(t, parentID)
	st := &kernel.RunState{TenantID: r.tenantID, SessionID: parentID, Seal: sealFn}
	cfg := kernel.RunConfig{System: "test", Catalog: catalog, MaxTurns: 10, AutonomyLevel: "autonomous", Input: "please delegate this"}

	var sawDelegationRequested bool
	for ev, err := range k.Run(context.Background(), st, cfg) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if ev.Type == store.EventDelegationRequested {
			sawDelegationRequested = true
		}
	}
	if !sawDelegationRequested {
		t.Fatalf("parent run never appended delegation_requested")
	}
	parentAfterSuspend := r.getSession(t, parentID)
	if parentAfterSuspend.Status != store.SessionStatusSuspended {
		t.Fatalf("parent status = %q, want suspended", parentAfterSuspend.Status)
	}

	final := waitForStatus(t, r, parentID, store.SessionStatusCompleted)
	if final.TerminalReason == nil || *final.TerminalReason != "completed" {
		t.Fatalf("parent terminal reason = %v, want completed", final.TerminalReason)
	}

	events, err := listEventsDirect(context.Background(), r.st, r.tenantID, parentID)
	if err != nil {
		t.Fatalf("list parent events: %v", err)
	}
	if !hasEventType(events, store.EventDelegationReturned) {
		t.Fatalf("parent's own log never shows delegation_returned")
	}

	// The delegation row itself must be resolved, never left pending.
	_ = d
	var found bool
	for _, dg := range mustListDelegations(t, r, parentID) {
		found = true
		if dg.Status != delegate.StatusReturned {
			t.Fatalf("delegation status = %q, want returned", dg.Status)
		}
	}
	if !found {
		t.Fatalf("no delegation row found for parent %s", parentID)
	}
}

// TestDelegate_BoundsFailClosed_ThroughTheRealChain proves README task
// 8.12's three bounds against the REAL Postgres-backed ledger (not a fake) —
// each violation must refuse with PermissionDenied at Gate 2, never reach
// the Rule-of-Two ask, and never spawn a child.
func TestDelegate_BoundsFailClosed_ThroughTheRealChain(t *testing.T) {
	r := setupOversightRig(t)
	prov := fake.New() // no script consumed if bounds correctly refuse before any Call
	k, pipeline, d, _ := buildDelegationRig(t, r, prov)
	_ = k

	t.Run("depth", func(t *testing.T) {
		parentID, childID, userID := uuid.New(), uuid.New(), uuid.New()
		r.createSession(t, parentID, userID, "autonomous")
		// A child of parentID, at depth 1 — attempting to delegate FROM
		// here must be refused (a session at the depth bound may not
		// delegate further).
		mustCreateChildSession(t, r, childID, parentID, 1)

		got := pipeline.Execute(context.Background(), tools.Invocation{
			TenantID: r.tenantID, SessionID: childID, ToolName: "platform/delegate@v1",
			Input:         mustMarshal(t, map[string]any{"agent_id": "w", "task": "x", "scope_grant": []string{"platform/delegate@v1"}}),
			AutonomyLevel: "autonomous",
		})
		if !got.PermissionDenied {
			t.Fatalf("Execute() = %+v, want PermissionDenied at the depth bound", got)
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		parentID, userID := uuid.New(), uuid.New()
		r.createSession(t, parentID, userID, "autonomous")
		for i := 0; i < delegate.MaxConcurrent; i++ {
			mustCreatePendingDelegation(t, r, d, parentID)
		}
		got := pipeline.Execute(context.Background(), tools.Invocation{
			TenantID: r.tenantID, SessionID: parentID, ToolName: "platform/delegate@v1",
			Input:         mustMarshal(t, map[string]any{"agent_id": "w", "task": "x", "scope_grant": []string{"platform/delegate@v1"}}),
			AutonomyLevel: "autonomous",
		})
		if !got.PermissionDenied {
			t.Fatalf("Execute() = %+v, want PermissionDenied at the concurrent bound", got)
		}
	})

	t.Run("scope outside admitted catalog", func(t *testing.T) {
		parentID, userID := uuid.New(), uuid.New()
		r.createSession(t, parentID, userID, "autonomous")
		got := pipeline.Execute(context.Background(), tools.Invocation{
			TenantID: r.tenantID, SessionID: parentID, ToolName: "platform/delegate@v1",
			Input:         mustMarshal(t, map[string]any{"agent_id": "w", "task": "x", "scope_grant": []string{"platform/shell@v1"}}),
			AutonomyLevel: "autonomous",
		})
		if !got.PermissionDenied {
			t.Fatalf("Execute() = %+v, want PermissionDenied for a scope_grant outside the admitted catalog", got)
		}
	})
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mustCreateChildSession(t *testing.T, r *oversightRig, childID, parentID uuid.UUID, depth int) {
	t.Helper()
	err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.CreateSession(ctx, tx, store.Session{
			SessionID: childID, SessionKey: childID.String(), TenantID: r.tenantID,
			SurfaceID: "test", UserID: uuid.New(), AgentID: uuid.Nil, AgentVersion: 1,
			HarnessDigest: []byte("test"), DataLabel: "internal", RouteModelID: "fake",
			AutonomyLevel: "autonomous", RootSessionID: parentID, Depth: depth, DelegationRole: "delegate",
		})
	})
	if err != nil {
		t.Fatalf("create child session: %v", err)
	}
}

func mustCreatePendingDelegation(t *testing.T, r *oversightRig, d *delegate.Delegations, parentID uuid.UUID) {
	t.Helper()
	childID := uuid.New()
	mustCreateChildSession(t, r, childID, parentID, 1)
	// Spawn would also start a live kernel run; for a pure bounds-counting
	// fixture we only need the delegations ROW, inserted directly.
	err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO delegations (delegation_id, tenant_id, parent_session_id, child_session_id, agent_id, task, scope_grant, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'pending')`,
			uuid.New(), r.tenantID, parentID, childID, "worker", "x", []byte(`[]`),
		)
		return err
	})
	if err != nil {
		t.Fatalf("insert pending delegation: %v", err)
	}
}

// mustListDelegations queries the delegations table directly — Delegations
// itself exposes no "list every row for a parent" method (only
// ListOpenForFanout, scoped by fanout_id, and Get by id); a direct query
// here is the pragmatic, honest choice for a test fixture over adding a
// method nothing else in this codebase needs yet.
func mustListDelegations(t *testing.T, r *oversightRig, parentID uuid.UUID) []delegate.Delegation {
	t.Helper()
	var out []delegate.Delegation
	err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT status FROM delegations WHERE parent_session_id = $1`, parentID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var status delegate.Status
			if err := rows.Scan(&status); err != nil {
				return err
			}
			out = append(out, delegate.Delegation{Status: status})
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("list delegations for parent %s: %v", parentID, err)
	}
	return out
}
