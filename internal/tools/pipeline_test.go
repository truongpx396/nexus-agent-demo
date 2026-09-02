package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/hooks"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions/safety"
)

// fakeTool is a fully scriptable Tool for pipeline tests — no builtin tool
// is exercised here on purpose, so a pipeline bug and a builtin-tool bug
// never masquerade as each other.
type fakeTool struct {
	ref             ToolRef
	desc            Descriptor
	taint           Taint
	concurrencySafe bool
	checkPerm       PermissionResult
	validateErr     error

	mu         sync.Mutex
	callCount  int
	lastInput  json.RawMessage
	callResult Result
	callErr    error
	callFn     func(input json.RawMessage) (Result, error) // overrides callResult/callErr when set
}

func (f *fakeTool) ID() ToolRef                            { return f.ref }
func (f *fakeTool) Descriptor() Descriptor                 { return f.desc }
func (f *fakeTool) Taint() Taint                           { return f.taint }
func (f *fakeTool) IsConcurrencySafe(json.RawMessage) bool { return f.concurrencySafe }
func (f *fakeTool) CheckPermissions(context.Context, json.RawMessage, RunContext) PermissionResult {
	return f.checkPerm
}
func (f *fakeTool) ValidateInput(context.Context, json.RawMessage, RunContext) error {
	return f.validateErr
}
func (f *fakeTool) Call(_ context.Context, input json.RawMessage, _ RunContext) (Result, error) {
	f.mu.Lock()
	f.callCount++
	f.lastInput = input
	f.mu.Unlock()
	if f.callFn != nil {
		return f.callFn(input)
	}
	return f.callResult, f.callErr
}

// alwaysDeferModel stands in for a real safety model leg in tests: rules
// still catch the obviously bad patterns (safety.DefaultRules), but nothing
// falls closed to Ask just because no real model is configured — that
// would make every test that isn't specifically about the safety layer
// depend on Gate 3's failure behavior instead of its own.
type alwaysDeferModel struct{}

func (alwaysDeferModel) Classify(context.Context, string, string) (safety.Verdict, string, error) {
	return safety.VerdictDefer, "test default", nil
}

func newFakeTool(ns, name string, effect EffectClass) *fakeTool {
	ref := ToolRef{Namespace: ns, Name: name, Version: "v1"}
	return &fakeTool{
		ref:             ref,
		desc:            Descriptor{ID: ref, Description: "a fake tool for tests", InputSchema: json.RawMessage(`{}`), EffectClass: effect},
		taint:           Taint{},
		concurrencySafe: true,
		checkPerm:       PermissionResult{Decision: "defer"},
		callResult:      Result{Output: json.RawMessage(`{"ok":true}`)},
	}
}

// testHarness bundles a Pipeline with everything needed to register and
// admit one fakeTool and resolve permissions permissively by default —
// individual tests mutate cfg fields (autonomy, hook configs, deny rules)
// before calling buildPipeline.
type testHarness struct {
	reg   *Registry
	tool  *fakeTool
	cfg   PipelineConfig
	blobs string
}

func newHarness(t *testing.T, tool *fakeTool) *testHarness {
	t.Helper()
	reg := NewRegistry()
	if err := reg.DeclareNamespace(tool.ref.Namespace, "test-owner"); err != nil {
		t.Fatalf("DeclareNamespace: %v", err)
	}
	if err := reg.Register(tool); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.SetAdmissionStatus(tool.ref, AdmissionClean); err != nil {
		t.Fatalf("SetAdmissionStatus: %v", err)
	}
	manifest := BuildManifest(reg)

	chain := permissions.NewChain(permissions.ChainConfig{
		Profiles: permissions.ProfileSet{Profiles: []permissions.ToolProfile{permissions.NewToolProfile("default", 1, tool.ref.String())}},
		Safety:   safety.NewClassifier(safety.DefaultRules(), alwaysDeferModel{}, 0),
	})

	blobDir := t.TempDir()
	return &testHarness{
		reg:   reg,
		tool:  tool,
		blobs: blobDir,
		cfg: PipelineConfig{
			Registry: reg,
			Manifest: manifest,
			Chain:    chain,
			Hooks:    hooks.NewDispatcher(),
			Blobs:    BlobStore{Dir: blobDir},
		},
	}
}

func (h *testHarness) pipeline() *Pipeline { return NewPipeline(h.cfg) }

func (h *testHarness) invocation(autonomy string, input string) Invocation {
	return Invocation{
		TenantID:      uuid.New(),
		SessionID:     uuid.New(),
		ToolName:      h.tool.ref.String(),
		Input:         json.RawMessage(input),
		AutonomyLevel: autonomy,
	}
}

func TestPipeline_HappyPathAllows(t *testing.T) {
	tool := newFakeTool("platform", "read_file", EffectClassReadOnly)
	h := newHarness(t, tool)
	p := h.pipeline()

	got := p.Execute(context.Background(), h.invocation("read_only", `{"path":"a.txt"}`))
	if got.IsError || got.PermissionDenied || got.AwaitingApproval {
		t.Fatalf("Execute() = %+v, want a clean success", got)
	}
	if string(got.Output) != `{"ok":true}` {
		t.Fatalf("Output = %s, want the tool's raw result", got.Output)
	}
	if tool.callCount != 1 {
		t.Fatalf("tool called %d times, want 1", tool.callCount)
	}
}

func TestPipeline_UnknownTool(t *testing.T) {
	tool := newFakeTool("platform", "read_file", EffectClassReadOnly)
	h := newHarness(t, tool)
	p := h.pipeline()

	inv := h.invocation("autonomous", `{}`)
	inv.ToolName = "platform/does_not_exist@v1"
	got := p.Execute(context.Background(), inv)
	if !got.IsError || !strings.Contains(got.Reason, "unknown_tool") {
		t.Fatalf("Execute() = %+v, want an unknown_tool error", got)
	}
}

func TestPipeline_DescriptorDriftIsRefused(t *testing.T) {
	tool := newFakeTool("platform", "read_file", EffectClassReadOnly)
	h := newHarness(t, tool)
	p := h.pipeline()

	// The manifest already pinned the original description; mutate it
	// live, simulating a catalog that changed shape mid-session.
	tool.desc.Description = "a completely different tool now"

	got := p.Execute(context.Background(), h.invocation("autonomous", `{}`))
	if !got.IsError || !strings.Contains(got.Reason, "descriptor_drift") {
		t.Fatalf("Execute() = %+v, want a descriptor_drift error", got)
	}
}

func TestPipeline_AdmissionStatusChangedAfterPinIsRefused(t *testing.T) {
	tool := newFakeTool("platform", "read_file", EffectClassReadOnly)
	h := newHarness(t, tool)
	p := h.pipeline()

	if err := h.reg.SetAdmissionStatus(tool.ref, AdmissionFlagged); err != nil {
		t.Fatalf("SetAdmissionStatus: %v", err)
	}
	got := p.Execute(context.Background(), h.invocation("autonomous", `{}`))
	if !got.IsError || !strings.Contains(got.Reason, "admission_flagged") {
		t.Fatalf("Execute() = %+v, want an admission_flagged error", got)
	}
}

func TestPipeline_InputValidationFails(t *testing.T) {
	tool := newFakeTool("platform", "read_file", EffectClassReadOnly)
	tool.validateErr = context.DeadlineExceeded // any non-nil error
	h := newHarness(t, tool)
	p := h.pipeline()

	got := p.Execute(context.Background(), h.invocation("autonomous", `{}`))
	if !got.IsError || !strings.Contains(got.Reason, "invalid_input") {
		t.Fatalf("Execute() = %+v, want an invalid_input error", got)
	}
}

func TestPipeline_Gate2DenyProducesPermissionDenied(t *testing.T) {
	tool := newFakeTool("platform", "read_file", EffectClassReadOnly)
	tool.checkPerm = PermissionResult{Decision: "deny", Reason: "tool policy refuses"}
	h := newHarness(t, tool)
	p := h.pipeline()

	got := p.Execute(context.Background(), h.invocation("autonomous", `{}`))
	if !got.PermissionDenied {
		t.Fatalf("Execute() = %+v, want PermissionDenied", got)
	}
}

func TestPipeline_AutonomyDeniesMutatingAtReadOnly(t *testing.T) {
	tool := newFakeTool("platform", "shell", EffectClassMutating)
	h := newHarness(t, tool)
	p := h.pipeline()

	got := p.Execute(context.Background(), h.invocation("read_only", `{"cmd":"echo hi"}`))
	if !got.PermissionDenied {
		t.Fatalf("Execute() = %+v, want PermissionDenied at read_only autonomy", got)
	}
	if tool.callCount != 0 {
		t.Fatalf("tool called %d times, want 0 (a denied call must never execute)", tool.callCount)
	}
}

// TestPipeline_SupervisedAsksForMutating is the Phase 3 "governed agent"
// demo behavior: a mutating effect at supervised autonomy suspends on an
// approval (README.md §5's Phase 3 demo line), surfaced here as
// AwaitingApproval since Phase 3 ships no oversight service to act on it.
func TestPipeline_SupervisedAsksForMutating(t *testing.T) {
	tool := newFakeTool("platform", "shell", EffectClassMutating)
	h := newHarness(t, tool)
	p := h.pipeline()

	got := p.Execute(context.Background(), h.invocation("supervised", `{"cmd":"echo hi"}`))
	if !got.AwaitingApproval {
		t.Fatalf("Execute() = %+v, want AwaitingApproval at supervised autonomy", got)
	}
	if got.AskKind != string(permissions.AskOnce) {
		t.Fatalf("AskKind = %q, want %q", got.AskKind, permissions.AskOnce)
	}
	if tool.callCount != 0 {
		t.Fatalf("tool called %d times, want 0 (an outstanding ask must never execute)", tool.callCount)
	}
}

func TestPipeline_HookDenyProducesPermissionDenied(t *testing.T) {
	tool := newFakeTool("platform", "read_file", EffectClassReadOnly)
	h := newHarness(t, tool)
	h.cfg.HookConfigs = []hooks.Config{
		{Name: "block-all", Event: hooks.PreToolUse, Kind: hooks.KindPrompt, Matcher: "*", PromptDecision: hooks.Deny, PromptTemplate: "blocked for test"},
	}
	p := h.pipeline()

	got := p.Execute(context.Background(), h.invocation("autonomous", `{}`))
	if !got.PermissionDenied {
		t.Fatalf("Execute() = %+v, want PermissionDenied from the hook", got)
	}
}

// TestPipeline_HookRewriteRebindsDigestAndFlowsToCall proves step 8: a hook
// that validly rewrites input hands the REWRITTEN input to Tool.Call, not
// the original — the whole point of "re-binds the digest" is that
// everything downstream of the hook sees one consistent value.
func TestPipeline_HookRewriteRebindsDigestAndFlowsToCall(t *testing.T) {
	tool := newFakeTool("platform", "read_file", EffectClassReadOnly)
	h := newHarness(t, tool)
	h.cfg.HookConfigs = []hooks.Config{
		{
			Name: "rewriter", Event: hooks.PreToolUse, Kind: hooks.KindCommand, Matcher: "*",
			UpdatablePaths: []string{"path"},
			Command:        "/bin/sh",
			Args:           []string{"-c", `cat >/dev/null; echo '{"decision":"defer","updated_tool_input":{"path":"/workspace/rewritten.txt"}}'`},
		},
	}
	p := h.pipeline()

	got := p.Execute(context.Background(), h.invocation("autonomous", `{"path":"/workspace/original.txt"}`))
	if got.IsError || got.PermissionDenied {
		t.Fatalf("Execute() = %+v, want success", got)
	}
	if string(tool.lastInput) != `{"path":"/workspace/rewritten.txt"}` {
		t.Fatalf("tool saw input %s, want the hook's rewritten input", tool.lastInput)
	}
}

func TestPipeline_PostHookDenyWithholdsOutput(t *testing.T) {
	tool := newFakeTool("platform", "read_file", EffectClassReadOnly)
	h := newHarness(t, tool)
	h.cfg.HookConfigs = []hooks.Config{
		{Name: "redact", Event: hooks.PostToolUse, Kind: hooks.KindPrompt, Matcher: "*", PromptDecision: hooks.Deny, PromptTemplate: "sensitive"},
	}
	p := h.pipeline()

	got := p.Execute(context.Background(), h.invocation("autonomous", `{}`))
	if !got.IsError || !strings.Contains(got.Reason, "withheld") {
		t.Fatalf("Execute() = %+v, want output withheld by the post_tool_use hook", got)
	}
}

func TestPipeline_ToolPanicIsRecovered(t *testing.T) {
	tool := newFakeTool("platform", "read_file", EffectClassReadOnly)
	tool.callFn = func(json.RawMessage) (Result, error) { panic("boom") }
	h := newHarness(t, tool)
	p := h.pipeline()

	got := p.Execute(context.Background(), h.invocation("autonomous", `{}`))
	if !got.IsError || !strings.Contains(got.Reason, "panicked") {
		t.Fatalf("Execute() = %+v, want a recovered-panic error", got)
	}
}

func TestPipeline_ResultBudgetingSpillsOversizedOutput(t *testing.T) {
	tool := newFakeTool("platform", "read_file", EffectClassReadOnly)
	big := strings.Repeat("x", maxResultBytes+1000)
	tool.callResult = Result{Output: json.RawMessage(`"` + big + `"`)}
	h := newHarness(t, tool)
	p := h.pipeline()

	got := p.Execute(context.Background(), h.invocation("autonomous", `{}`))
	if got.IsError {
		t.Fatalf("Execute() = %+v, want success (budgeting is not a failure)", got)
	}
	var decoded budgetedResult
	if err := json.Unmarshal(got.Output, &decoded); err != nil {
		t.Fatalf("Output is not a budgetedResult: %v (%s)", err, got.Output)
	}
	if !decoded.Truncated || !strings.Contains(decoded.Preview, "do not infer success from the preview") {
		t.Fatalf("decoded = %+v, want Truncated with the standard banner", decoded)
	}
	if _, err := os.Stat(decoded.FullResultPath); err != nil {
		t.Fatalf("full result blob missing: %v", err)
	}
	if filepath.Dir(decoded.FullResultPath) != h.blobs {
		t.Fatalf("blob path %s not under configured blob dir %s", decoded.FullResultPath, h.blobs)
	}
}

// TestPipeline_SerialGateSerializesAnUnsafeTool exercises step 12: many
// concurrent Execute calls against a NOT-concurrency-safe tool must never
// overlap inside Call, so a naive (unsynchronized) increment inside Call
// still comes out exactly right.
func TestPipeline_SerialGateSerializesAnUnsafeTool(t *testing.T) {
	tool := newFakeTool("platform", "counter", EffectClassReadOnly)
	tool.concurrencySafe = false
	counter := 0
	tool.callFn = func(json.RawMessage) (Result, error) {
		local := counter
		local++
		counter = local // unsynchronized on purpose: only correct if Execute serializes calls
		return Result{Output: json.RawMessage(`{}`)}, nil
	}
	h := newHarness(t, tool)
	p := h.pipeline()
	inv := h.invocation("autonomous", `{}`)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			p.Execute(context.Background(), inv)
		}()
	}
	wg.Wait()

	if counter != n {
		t.Fatalf("counter = %d, want %d (concurrent calls were not serialized)", counter, n)
	}
}

// TestPipeline_ExecuteApprovedHappyPath is README task 5.7/5.8's resume
// path: the digest a supervised Ask produced (TestPipeline_
// SupervisedAsksForMutating's own CanonicalDigest) is exactly what a human
// approval binds to and ExecuteApproved re-verifies — presented back
// unchanged, it must execute, skipping the chain entirely (a human already
// decided out of band).
func TestPipeline_ExecuteApprovedHappyPath(t *testing.T) {
	tool := newFakeTool("platform", "shell", EffectClassMutating)
	h := newHarness(t, tool)
	p := h.pipeline()
	inv := h.invocation("supervised", `{"cmd":"echo hi"}`)

	asked := p.Execute(context.Background(), inv)
	if !asked.AwaitingApproval || len(asked.CanonicalDigest) == 0 {
		t.Fatalf("Execute() = %+v, want AwaitingApproval with a CanonicalDigest", asked)
	}

	got := p.ExecuteApproved(context.Background(), inv, asked.CanonicalDigest)
	if got.IsError || got.ApprovalMismatch {
		t.Fatalf("ExecuteApproved() = %+v, want a clean success", got)
	}
	if tool.callCount != 1 {
		t.Fatalf("tool called %d times, want 1", tool.callCount)
	}
}

// TestPipeline_ExecuteApprovedMismatchRefuses is the adversarial case
// (README task 5.7, SC-025): a digest that does not match what was
// actually approved — simulating either a bug upstream or a deliberate
// substitution attempt after grant — must refuse, typed, and must NEVER
// execute the tool. "Never a silent re-request" means this is the ONLY
// outcome: no fallback to asking again, no partial execution.
func TestPipeline_ExecuteApprovedMismatchRefuses(t *testing.T) {
	tool := newFakeTool("platform", "shell", EffectClassMutating)
	h := newHarness(t, tool)
	p := h.pipeline()
	inv := h.invocation("supervised", `{"cmd":"echo hi"}`)

	asked := p.Execute(context.Background(), inv)
	if !asked.AwaitingApproval {
		t.Fatalf("Execute() = %+v, want AwaitingApproval", asked)
	}

	tamperedDigest := []byte("this-is-not-the-approved-digest")
	got := p.ExecuteApproved(context.Background(), inv, tamperedDigest)
	if !got.ApprovalMismatch {
		t.Fatalf("ExecuteApproved() = %+v, want ApprovalMismatch", got)
	}
	if !got.IsError {
		t.Fatal("ApprovalMismatch result must also be IsError — never a quiet success")
	}
	if tool.callCount != 0 {
		t.Fatalf("tool called %d times, want 0 (a mismatched digest must never execute)", tool.callCount)
	}

	// ExecuteApproved is unreachable without SOME digest to check against —
	// there is no standing-scope-style shortcut into it (README §5's Phase
	// 5 adversarial test, SC-025): the only way to reach a clean execution
	// is to already know the exact digest a human approved.
	empty := p.ExecuteApproved(context.Background(), inv, nil)
	if !empty.ApprovalMismatch || tool.callCount != 0 {
		t.Fatalf("ExecuteApproved(nil digest) = %+v, want ApprovalMismatch and zero calls", empty)
	}
}

// TestPipeline_AwaitingChildSessionIDShortCircuits is README task 8.10's
// own pipeline-level seam: a Tool.Call that sets
// Result.AwaitingChildSessionID (platform/delegate, internal/tools/builtin)
// must skip PostToolUse hooks and result budgeting entirely and translate
// straight into ExecuteResult.AwaitingDelegation — the same short-circuit
// shape AwaitingApproval already gets one layer up, just triggered AFTER
// Call runs instead of before it.
func TestPipeline_AwaitingChildSessionIDShortCircuits(t *testing.T) {
	childID := uuid.New()
	tool := newFakeTool("platform", "delegate", EffectClassMutating)
	tool.callResult = Result{Output: json.RawMessage(`{"status":"delegated"}`), AwaitingChildSessionID: &childID}
	h := newHarness(t, tool)
	p := h.pipeline()
	inv := h.invocation("autonomous", `{}`)

	got := p.Execute(context.Background(), inv)
	if !got.AwaitingDelegation {
		t.Fatalf("Execute() = %+v, want AwaitingDelegation", got)
	}
	if got.ChildSessionID != childID {
		t.Fatalf("ChildSessionID = %s, want %s", got.ChildSessionID, childID)
	}
	if got.IsError {
		t.Fatalf("Execute() = %+v, an awaiting-delegation result must not also be an error", got)
	}
}

// TestPipeline_FoldTaint_CopyAtSpawnAndFoldOnReturn exercises the two
// moments README task 8.11 needs from Pipeline directly (internal/delegate
// is what actually calls these, at spawn time and at resolve time — this
// test isolates the mechanism itself from that package's own DB-backed
// machinery).
func TestPipeline_FoldTaint_CopyAtSpawnAndFoldOnReturn(t *testing.T) {
	tool := newFakeTool("platform", "noop", EffectClassReadOnly)
	h := newHarness(t, tool)
	p := h.pipeline()

	parent, child := uuid.New(), uuid.New()
	if got := p.TaintStateFor(parent); got != ([3]bool{}) {
		t.Fatalf("a session nobody ever touched should start all-false, got %v", got)
	}

	// Copy-at-spawn: the parent already engaged the untrusted-input leg;
	// the child must start with that SAME leg engaged.
	p.FoldTaint(parent, [3]bool{true, false, false})
	p.FoldTaint(child, p.TaintStateFor(parent))
	if got := p.TaintStateFor(child); got != ([3]bool{true, false, false}) {
		t.Fatalf("child taint after copy-at-spawn = %v, want the parent's own state copied in", got)
	}

	// Fold-on-return: the child ALSO touched private data while it ran —
	// folding it into the parent must ADD that leg, never clear the one
	// the parent already had (task 8.11: "a summary never clears the
	// untrusted leg").
	p.FoldTaint(child, [3]bool{false, true, false})
	p.FoldTaint(parent, p.TaintStateFor(child))
	if got := p.TaintStateFor(parent); got != ([3]bool{true, true, false}) {
		t.Fatalf("parent taint after fold-on-return = %v, want both legs engaged", got)
	}
}
