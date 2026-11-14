//go:build integration

// A run that is resumed — after an approval, after a delegation returns — must
// carry the configuration its session pinned when it was created: the autonomy
// level, the model it was routed to, and whether it is conversational. Four
// services resume a run (runctl, oversight, delegate, plan) and each used to
// rebuild kernel.RunConfig by hand from the session row, so each took a different
// subset of those fields. These tests drive the real services against a real
// Postgres and check the resumed turn behaves exactly as the first turn did.
//
// Shares oversightRig, newFileWritePipeline and buildDelegationRig with
// oversight_test.go and orchestration_test.go (same package).
package integration

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent/internal/cost"
	"github.com/truongpx396/nexus-agent/internal/eventlog"
	"github.com/truongpx396/nexus-agent/internal/eventlog/postgres"
	"github.com/truongpx396/nexus-agent/internal/oversight"
	oversightpg "github.com/truongpx396/nexus-agent/internal/oversight/postgres"
	"github.com/truongpx396/nexus-agent/internal/provider"
	"github.com/truongpx396/nexus-agent/internal/provider/fake"
	"github.com/truongpx396/nexus-agent/internal/runctl"
	runctlpg "github.com/truongpx396/nexus-agent/internal/runctl/postgres"
	"github.com/truongpx396/nexus-agent/internal/store"
	"github.com/truongpx396/nexus-agent/internal/tools/kerneladapter"
	"github.com/truongpx396/nexus-agent/kernel"
)

// routedModel is the model a session in these tests was routed to at creation. It
// is deliberately not "fake", the id oversightRig.createSession gives every
// session, so a resume that drops the field cannot pass by luck.
const routedModel = "routed-model"

// modelRecorder is a kernel.BudgetGate that remembers which model each turn asked
// to reserve against, which is what the cost ledger prices the turn under.
type modelRecorder struct {
	mu     sync.Mutex
	models []string
}

func (m *modelRecorder) Reserve(ctx context.Context, req cost.ReserveRequest) (cost.Reservation, error) {
	m.mu.Lock()
	m.models = append(m.models, req.ModelID)
	m.mu.Unlock()
	return kernel.NoopBudgetGate{}.Reserve(ctx, req)
}

func (m *modelRecorder) Reconcile(context.Context, cost.Reservation, provider.Usage, bool) error {
	return nil
}

func (m *modelRecorder) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.models...)
}

// createConversationalSession creates a supervised, conversational session routed
// to routedModel: the shape a Telegram, Zalo or email chat has.
func (r *oversightRig) createConversationalSession(t *testing.T, sessionID, userID uuid.UUID) {
	t.Helper()
	err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.CreateSession(ctx, tx, eventlog.Session{
			SessionID: sessionID, SessionKey: sessionID.String(), TenantID: r.tenantID,
			SurfaceID: "test", UserID: userID, AgentID: uuid.Nil, AgentVersion: 1,
			HarnessDigest: []byte("test-digest"), DataLabel: "internal", RouteModelID: routedModel,
			AutonomyLevel: "supervised", Conversational: true,
		})
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
}

// TestOversightResume_KeepsTheSessionsPinnedRunConfig: a conversational chat asks
// to write a file, a human approves, and the run resumes through
// oversight.Resumer. The reply that follows the approval must pause the session
// for the next message exactly as the reply before it would have, not end it, and
// the turn must be priced under the model the session was routed to.
func TestOversightResume_KeepsTheSessionsPinnedRunConfig(t *testing.T) {
	rig := setupOversightRig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()

	pipeline, catalog := newFileWritePipeline(t, t.TempDir())
	approvals := oversight.NewApprovals(oversightpg.New(rig.st, rig.keys, rig.chain))
	prov := fake.New(
		fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "tool_use", ToolUseID: "tu1", ToolName: "platform/file_write@v1", Input: `{"path":"out.txt","content":"hello"}`},
			{Kind: "done", Done: "stop"},
		}},
		contentScript("written, anything else?"),
	)
	budget := &modelRecorder{}
	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{prov}), Tools: kerneladapter.PipelineExecutor{Pipeline: pipeline},
		Budget: budget,
		Log:    postgres.New(rig.st, postgres.Hooks{Chain: rig.chain, Suspend: rig.onSuspend(approvals)}),
	}

	rig.createConversationalSession(t, sessionID, userID)
	cfg := kernel.RunConfig{
		System: "test", Catalog: catalog, ModelID: routedModel, MaxTurns: 10,
		AutonomyLevel: "supervised", Conversational: true, Input: "write out.txt",
	}
	runToCompletion(t, k, &kernel.RunState{TenantID: rig.tenantID, SessionID: sessionID, Seal: rig.seal(t, sessionID)}, cfg)
	if got := rig.getSession(t, sessionID).Status; got != eventlog.SessionStatusSuspended {
		t.Fatalf("status after the tool call = %q, want suspended on the approval", got)
	}

	pending, err := approvals.ListPending(ctx, rig.tenantID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPending: err=%v, count=%d", err, len(pending))
	}
	resumer := &oversight.Resumer{
		Kernel: k, Approvals: approvals, Sessions: oversightpg.New(rig.st, rig.keys, rig.chain), Keys: oversightpg.New(rig.st, rig.keys, rig.chain),
		System: "test", Catalog: catalog, MaxTurns: 10,
	}
	for _, err := range resumer.Grant(ctx, rig.tenantID, pending[0].ApprovalID, userID.String()) {
		if err != nil {
			t.Fatalf("Grant resume: %v", err)
		}
	}

	if got := rig.getSession(t, sessionID).Status; got != eventlog.SessionStatusAwaitingInput {
		t.Errorf("status after the approved turn = %q, want %q: a conversational session pauses for the next message, it does not end", got, eventlog.SessionStatusAwaitingInput)
	}
	if got, want := budget.seen(), []string{routedModel, routedModel}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("turns were metered under models %q, want %q: the resumed turn must run under the model the session was routed to", got, want)
	}
}

// TestDelegateResume_KeepsTheSessionsPinnedRunConfig: a conversational parent
// delegates, the child returns, and delegate resumes the parent. Its final reply
// pauses the session for the next message rather than ending it, and every turn,
// the spawned child's included, runs under the model the session was routed to.
func TestDelegateResume_KeepsTheSessionsPinnedRunConfig(t *testing.T) {
	r := setupOversightRig(t)
	prov := fake.New(
		delegateToolUse("worker", "summarize the notes", "platform/delegate@v1"),
		contentScript("child summary: all good"),
		contentScript("parent: anything else?"),
	)
	k, _, _, catalog := buildDelegationRig(t, r, prov, nil)
	budget := &modelRecorder{}
	k.Budget = budget // the delegation services hold this same *Kernel

	parentID, userID := uuid.New(), uuid.New()
	r.createConversationalSession(t, parentID, userID)
	cfg := kernel.RunConfig{
		System: "test", Catalog: catalog, ModelID: routedModel, MaxTurns: 10,
		AutonomyLevel: "supervised", Conversational: true, Input: "please delegate this",
	}
	runToCompletion(t, k, &kernel.RunState{TenantID: r.tenantID, SessionID: parentID, Seal: r.seal(t, parentID)}, cfg)

	// The child is spawned in the background and its return resumes the parent; a
	// conversational parent ends up awaiting the next message, not completed.
	waitForStatus(t, r, parentID, eventlog.SessionStatusAwaitingInput)
	// Three turns ran: the parent's first, the spawned child's, and the parent's
	// resumed turn. The child takes its config from the session row Spawn wrote
	// (runstate.ConfigFor), and that row copies the parent's route, so all three are
	// priced under the routed model.
	if seen := budget.seen(); len(seen) != 3 {
		t.Errorf("turns metered = %q, want three (parent, child, resumed parent)", seen)
	} else {
		for i, m := range seen {
			if m != routedModel {
				t.Errorf("turn %d metered under %q, want %q", i+1, m, routedModel)
			}
		}
	}
}

// callRecorder is a provider.Provider that remembers the system prompt and tool
// list of every call before handing it to a scripted provider.
type callRecorder struct {
	inner provider.Provider

	mu      sync.Mutex
	systems []string
	tools   [][]string
}

func (c *callRecorder) Stream(ctx context.Context, p provider.Prompt, tools []provider.ToolSchema, rc provider.RunContext) (provider.Stream, error) {
	names := make([]string, len(tools))
	for i, tl := range tools {
		names[i] = tl.Name
	}
	c.mu.Lock()
	c.systems = append(c.systems, p.System)
	c.tools = append(c.tools, names)
	c.mu.Unlock()
	return c.inner.Stream(ctx, p, tools, rc)
}

// TestRunCtlResume_ShowsTheModelTheMemoryAndToolsTheSessionStartedWith: the process
// that accepts a run only seeds it (kernelRunStarter.StartRun), and a queue worker
// in another process drives the turns through runctl.Control.Resume with the
// process-wide system prompt and catalog. Before the memory text and the run's MCP
// tool schemas were recorded in the log and read back, that worker's prompt had
// neither, although the log said memory_loaded and tool_loaded. This drives both
// halves against a real Postgres, with a second kernel and a fresh Control sharing
// nothing in memory with the first.
func TestRunCtlResume_ShowsTheModelTheMemoryAndToolsTheSessionStartedWith(t *testing.T) {
	rig := setupOversightRig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	rig.createSession(t, sessionID, userID, "supervised")

	resident := provider.ToolSchema{Name: "platform/file_write@v1", Description: "write a file", InputSchema: json.RawMessage(`{"type":"object"}`)}
	mcp := provider.ToolSchema{Name: "mcp/github/list_prs@v1", Description: "list pull requests", InputSchema: json.RawMessage(`{"type":"object","properties":{"repo":{"type":"string"}}}`)}

	// The accepting process: seed only, with the memory and the MCP tool on the config.
	starter := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fake.New()}), Tools: kernel.NotImplementedToolExecutor{},
		Budget: kernel.NoopBudgetGate{}, Log: postgres.New(rig.st, postgres.Hooks{}),
	}
	seedCfg := kernel.RunConfig{
		System: "base system", Catalog: []provider.ToolSchema{resident}, ModelID: "fake", MaxTurns: 5,
		Input: "list the open pull requests", AutonomyLevel: "supervised",
		LoadedTools:   []string{resident.Name, mcp.Name},
		MemorySources: []string{"notes.md"}, MemoryText: "the tenant prefers metric units",
		ExtraCatalog: []provider.ToolSchema{mcp},
	}
	state := &kernel.RunState{TenantID: rig.tenantID, SessionID: sessionID, Seal: rig.seal(t, sessionID)}
	for _, err := range starter.Seed(ctx, state, seedCfg) {
		if err != nil {
			t.Fatalf("Seed: %v", err)
		}
	}

	// The worker: its own kernel and Control, configured with only what every process holds.
	rec := &callRecorder{inner: fake.New(contentScript("done"))}
	worker := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{rec}), Tools: kernel.NotImplementedToolExecutor{},
		Budget: kernel.NoopBudgetGate{}, Log: postgres.New(rig.st, postgres.Hooks{}),
	}
	ctl := &runctl.Control{
		Store: runctlpg.New(rig.st, rig.keys, nil), Kernel: worker,
		System: "base system", Catalog: []provider.ToolSchema{resident}, MaxTurns: 5,
	}
	for _, err := range ctl.Resume(ctx, rig.tenantID, sessionID) {
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
	}

	if len(rec.systems) != 1 {
		t.Fatalf("the worker called the model %d times, want once", len(rec.systems))
	}
	if want := "base system\n\nthe tenant prefers metric units"; rec.systems[0] != want {
		t.Errorf("the worker's system prompt = %q, want %q", rec.systems[0], want)
	}
	if want := []string{resident.Name, mcp.Name}; !slices.Equal(rec.tools[0], want) {
		t.Errorf("the worker's tool list = %v, want %v", rec.tools[0], want)
	}
}
