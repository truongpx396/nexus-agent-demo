//go:build integration

// Durable Rule-of-Two taint state (kernel.TaintSeeder, kernel.RehydrateTaint,
// tools.Pipeline.SeedTaint) closes a real gap: Pipeline.TaintStateFor's own
// doc comment already admitted taint state lived ONLY in a process-lifetime
// in-memory cache, lost on any worker restart. This file's one test is the
// headline proof that mattered: taint engaged before a (simulated) restart
// must still count toward the Rule-of-Two budget after it.
package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions/safety"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// fakeTaintTool is a minimal tools.Tool with a fully scriptable, SINGLE-leg
// Taint declaration — no builtin tool isolates ReadsPrivateData alone (each
// either combines it with another leg or declares none at all), and this
// test needs precise control over which ONE leg each call engages.
// EffectClassReadOnly throughout keeps the test scoped to Rule-of-Two
// itself: layer 3 (autonomy) always Defers a read-only effect regardless
// of the session's own autonomy level.
type fakeTaintTool struct {
	ref   tools.ToolRef
	taint tools.Taint
}

func (t fakeTaintTool) ID() tools.ToolRef { return t.ref }
func (t fakeTaintTool) Descriptor() tools.Descriptor {
	return tools.Descriptor{ID: t.ref, Description: "taint test fixture", InputSchema: json.RawMessage(`{}`), EffectClass: tools.EffectClassReadOnly}
}
func (t fakeTaintTool) Taint() tools.Taint                   { return t.taint }
func (fakeTaintTool) IsConcurrencySafe(json.RawMessage) bool { return true }
func (fakeTaintTool) CheckPermissions(context.Context, json.RawMessage, tools.RunContext) tools.PermissionResult {
	return tools.PermissionResult{Decision: "defer"}
}
func (fakeTaintTool) ValidateInput(context.Context, json.RawMessage, tools.RunContext) error {
	return nil
}
func (fakeTaintTool) Call(context.Context, json.RawMessage, tools.RunContext) (tools.Result, error) {
	return tools.Result{Output: json.RawMessage(`{"ok":true}`)}, nil
}

// newTaintTestPipeline builds a real *tools.Pipeline admitting three fixture
// tools — test/leg_a (untrusted input), test/leg_b (private data),
// test/leg_c (external effect) — each isolating exactly one Rule-of-Two
// leg. Calling this twice produces two INDEPENDENT Pipeline instances with
// independent Pipeline.sessions maps: exactly what simulating a process
// restart needs (a real restart doesn't re-run tool registration either —
// this just needs two Pipelines that share no in-memory state, which two
// separately-built ones already are).
func newTaintTestPipeline(t *testing.T) *tools.Pipeline {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.DeclareNamespace("test", "test-owner"); err != nil {
		t.Fatalf("declare namespace: %v", err)
	}
	var refs []string
	register := func(name string, taint tools.Taint) {
		tool := fakeTaintTool{ref: tools.ToolRef{Namespace: "test", Name: name, Version: "v1"}, taint: taint}
		if err := reg.Register(tool); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		if err := reg.SetAdmissionStatus(tool.ref, tools.AdmissionClean); err != nil {
			t.Fatalf("admit %s: %v", name, err)
		}
		refs = append(refs, tool.ref.String())
	}
	register("leg_a", tools.Taint{ReturnsUntrusted: true})
	register("leg_b", tools.Taint{ReadsPrivateData: true})
	register("leg_c", tools.Taint{MutatesExternal: true})

	manifest := tools.BuildManifest(reg)
	chain := permissions.NewChain(permissions.ChainConfig{
		Profiles: permissions.ProfileSet{Profiles: []permissions.ToolProfile{permissions.NewToolProfile("default", 1, refs...)}},
		// alwaysDeferModel (phase6_reliability_test.go, same package) — Gate
		// 3's safety classifier fails closed to Ask with no model leg
		// configured; this test's own concern is Rule-of-Two, not Gate 3.
		Safety: safety.NewClassifier(safety.DefaultRules(), alwaysDeferModel{}, 0),
	})
	return tools.NewPipeline(tools.PipelineConfig{Registry: reg, Manifest: manifest, Chain: chain})
}

// TestRuleOfTwo_TaintStateSurvivesASimulatedProcessRestart is the headline
// proof: two legs engaged by "process A" must still count toward the
// Rule-of-Two budget when "process B" — an entirely independent
// *tools.Pipeline/kernel.Kernel pair, sharing nothing in memory —
// resumes the SAME session and dispatches a third-leg call. Before
// kernel.TaintSeeder/RehydrateTaint existed, process B would have started
// that session's taint state at the zero value (a worker restart mid-run
// silently resetting Rule-of-Two enforcement to zero, per
// Pipeline.TaintStateFor's own pre-existing doc comment) and allowed the
// third leg outright instead of asking.
func TestRuleOfTwo_TaintStateSurvivesASimulatedProcessRestart(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID := uuid.New()
	sessionID := uuid.New()
	userID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("generate KEK: %v", err)
	}
	keys := crypto.NewKeyStore(kek)
	var dek crypto.DEK
	err = st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		dek, derr = keys.NewDEK(ctx, tx, tenantID)
		if derr != nil {
			return derr
		}
		return store.CreateSession(ctx, tx, store.Session{
			SessionID: sessionID, SessionKey: sessionID.String(), TenantID: tenantID,
			SurfaceID: "test", UserID: userID, AgentVersion: 1,
			HarnessDigest: []byte("test-digest"), DataLabel: "internal", RouteModelID: "test-model",
			AutonomyLevel: "autonomous", Conversational: true,
		})
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	seal := func(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", err
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}

	// Turn 1 (process A): two tool calls, each engaging a DIFFERENT leg —
	// two calls, not one tool declaring both, since Rule-of-Two only asks
	// once a THIRD distinct leg would be engaged. Turn 2: a plain reply,
	// pausing (Conversational) rather than terminating. Turn 3 (process B,
	// after "restart"): a third tool call engaging the LAST leg.
	fakeProvider := fake.New(
		fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "tool_use", ToolUseID: "tu1", ToolName: "test/leg_a@v1", Input: `{}`},
			{Kind: "tool_use", ToolUseID: "tu2", ToolName: "test/leg_b@v1", Input: `{}`},
			{Kind: "done", Done: "stop"},
		}},
		contentOnlyScript("okay, pausing here"),
		fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "tool_use", ToolUseID: "tu3", ToolName: "test/leg_c@v1", Input: `{}`},
			{Kind: "done", Done: "stop"},
		}},
	)

	pipelineA := newTaintTestPipeline(t)
	kA := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fakeProvider}),
		Tools:    kernel.PipelineExecutor{Pipeline: pipelineA},
		Budget:   kernel.NoopBudgetGate{},
		Store:    st,
	}
	runState := &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal}
	cfg := kernel.RunConfig{
		System: "test", ModelID: "test-model", MaxTurns: 5, Input: "hello",
		AutonomyLevel: "autonomous", Conversational: true,
	}
	for ev, err := range kA.Run(ctx, runState, cfg) {
		if err != nil {
			t.Fatalf("Run(): %v", err)
		}
		_ = ev
	}

	full, err := listEventsDirect(ctx, st, tenantID, sessionID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var taintTransitions int
	for _, e := range full {
		if e.Type == store.EventTaintTransition {
			taintTransitions++
		}
	}
	if taintTransitions != 2 {
		t.Fatalf("durable log has %d taint_transition events, want 2 (one per newly engaged leg)", taintTransitions)
	}

	// "Process B": a genuinely independent Pipeline/Kernel pair, sharing no
	// in-memory state with process A — fakeProvider is reused only because
	// it's simpler to author one ordered script list than wire up a second
	// provider instance; in reality this would be a shared external model
	// endpoint either process dials, never in-process state.
	pipelineB := newTaintTestPipeline(t)
	kB := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fakeProvider}),
		Tools:    kernel.PipelineExecutor{Pipeline: pipelineB},
		Budget:   kernel.NoopBudgetGate{},
		Store:    st,
	}
	ctl := &runctl.Control{Store: st, Keys: keys, Kernel: kB, System: "test", MaxTurns: 5}

	events, err := ctl.ResumeConversation(ctx, tenantID, sessionID, "one more thing")
	if err != nil {
		t.Fatalf("ResumeConversation refused: %v", err)
	}
	var awaitingApproval bool
	for ev, everr := range events {
		if everr != nil {
			t.Fatalf("ResumeConversation() yielded error: %v", everr)
		}
		if ev.Type == store.EventApprovalRequested {
			awaitingApproval = true
		}
	}
	if !awaitingApproval {
		t.Fatal("leg_c (the third Rule-of-Two leg) was allowed outright — the simulated restart silently reset the taint budget instead of restoring it from the durable log")
	}

	sess, err := getSessionDirect(ctx, st, tenantID, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Status != store.SessionStatusSuspended {
		t.Fatalf("session status = %q, want %q (suspended awaiting the third-leg approval)", sess.Status, store.SessionStatusSuspended)
	}
}
