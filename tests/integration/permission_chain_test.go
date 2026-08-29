//go:build integration

// This is the Phase 3 counterpart to rest_run_test.go: it proves the two
// new kernel/loop.go reactions Phase 3 adds around a ToolExecutor result
// (README.md §5's Phase 3 acceptance line) — a PermissionDenied result ends
// the run with the typed ReasonPermissionDenied terminal, and an
// AwaitingApproval result suspends it (EventApprovalRequested appended,
// session status "suspended", no terminal event) rather than terminating or
// continuing. It drives kernel.Kernel directly with a scripted
// kernel.ToolExecutor rather than the real internal/tools.Pipeline, so a
// failure here isolates the kernel's reaction from the permission chain's
// own (separately unit-tested in internal/permissions) decision logic.
package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/harness"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// scriptedToolExecutor always returns the same ToolResult, regardless of
// which tool_use it's given — enough to drive kernel/loop.go's dispatch
// reaction without any real permission chain behind it.
type scriptedToolExecutor struct {
	result kernel.ToolResult
}

func (s scriptedToolExecutor) Execute(context.Context, kernel.ToolUseRequest, kernel.ExecContext) kernel.ToolResult {
	return s.result
}

// newTestSession creates a tenant + session row and returns everything
// needed to drive one kernel.Run call against it directly (no REST surface
// involved — internal/surfaces/rest is Phase 2's concern, already covered
// by rest_run_test.go).
func newTestSession(t *testing.T, ctx context.Context, st *store.Store, autonomy string) (tenantID, sessionID uuid.UUID, seal kernel.SealFunc) {
	t.Helper()
	tenantID = uuid.New()
	sessionID = uuid.New()
	userID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("generate KEK: %v", err)
	}
	keyStore := crypto.NewKeyStore(kek)

	digest := harness.Digest(harness.Config{SystemPromptVersion: "phase3-test", PromptMode: "phase3-test"})
	var dek crypto.DEK
	err = st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		dek, derr = keyStore.NewDEK(ctx, tx, tenantID)
		if derr != nil {
			return derr
		}
		return store.CreateSession(ctx, tx, store.Session{
			SessionID:     sessionID,
			SessionKey:    sessionID.String(),
			TenantID:      tenantID,
			SurfaceID:     "test",
			UserID:        userID,
			AgentVersion:  1,
			HarnessDigest: digest,
			DataLabel:     string(provider.DataLabelInternal),
			RouteModelID:  "test-model",
			RouteReason:   map[string]string{"reason": "test"},
			AutonomyLevel: autonomy,
		})
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	seal = func(plaintext []byte) (sealed, digestBytes []byte, keyID string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", err
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}
	return tenantID, sessionID, seal
}

func toolCallScript() fake.Script {
	return fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "tool_use", ToolUseID: "tu1", ToolName: "platform/shell@v1", Input: `{"cmd":"rm -rf build/"}`},
		{Kind: "done", Done: "stop"},
	}}
}

func TestKernel_PermissionDeniedTerminatesRun(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID, sessionID, seal := newTestSession(t, ctx, st, "read_only")

	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fake.New(toolCallScript())}),
		Tools:    scriptedToolExecutor{result: kernel.ToolResult{IsError: true, PermissionDenied: true, Reason: "denied at layer autonomy: read_only permits read-only effects only"}},
		Budget:   kernel.NoopBudgetGate{},
		Store:    st,
	}
	runState := &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal}
	cfg := kernel.RunConfig{System: "test", ModelID: "test-model", MaxTurns: 5, Input: "delete the build dir", AutonomyLevel: "read_only"}

	var events []store.Event
	for ev, err := range k.Run(ctx, runState, cfg) {
		if err != nil {
			t.Fatalf("Run() yielded error: %v", err)
		}
		events = append(events, ev)
	}

	if len(events) == 0 || events[len(events)-1].Type != store.EventTerminal {
		t.Fatalf("last event type = %v, want terminal", events[len(events)-1].Type)
	}

	sess, err := getSessionDirect(ctx, st, tenantID, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Status != store.SessionStatusFailed {
		t.Fatalf("session status = %q, want %q", sess.Status, store.SessionStatusFailed)
	}
	if sess.TerminalReason == nil || *sess.TerminalReason != string(kernel.ReasonPermissionDenied) {
		t.Fatalf("terminal_reason = %v, want %q", sess.TerminalReason, kernel.ReasonPermissionDenied)
	}
}

func TestKernel_AwaitingApprovalSuspendsRun(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID, sessionID, seal := newTestSession(t, ctx, st, "supervised")

	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fake.New(toolCallScript())}),
		Tools:    scriptedToolExecutor{result: kernel.ToolResult{IsError: true, AwaitingApproval: true, AskKind: "once", Reason: "approval required at layer autonomy"}},
		Budget:   kernel.NoopBudgetGate{},
		Store:    st,
	}
	runState := &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal}
	cfg := kernel.RunConfig{System: "test", ModelID: "test-model", MaxTurns: 5, Input: "delete the build dir", AutonomyLevel: "supervised"}

	var events []store.Event
	for ev, err := range k.Run(ctx, runState, cfg) {
		if err != nil {
			t.Fatalf("Run() yielded error: %v", err)
		}
		events = append(events, ev)
	}

	if len(events) == 0 || events[len(events)-1].Type != store.EventApprovalRequested {
		t.Fatalf("last event type = %v, want approval_requested (no terminal event on a suspend)", events[len(events)-1].Type)
	}
	for _, ev := range events {
		if ev.Type == store.EventTerminal {
			t.Fatal("a suspended run must never append a terminal event")
		}
	}

	sess, err := getSessionDirect(ctx, st, tenantID, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Status != store.SessionStatusSuspended {
		t.Fatalf("session status = %q, want %q", sess.Status, store.SessionStatusSuspended)
	}
	if sess.TerminalReason != nil {
		t.Fatalf("terminal_reason = %v, want nil (a suspend is not a terminal state)", *sess.TerminalReason)
	}
}

func getSessionDirect(ctx context.Context, st *store.Store, tenantID, sessionID uuid.UUID) (store.Session, error) {
	var sess store.Session
	err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var gerr error
		sess, gerr = store.GetSession(ctx, tx, sessionID)
		return gerr
	})
	return sess, err
}
