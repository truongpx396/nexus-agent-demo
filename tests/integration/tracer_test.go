//go:build integration

// Proves kernel/loop.go's Tracer instrumentation (docs/local-llm.md)
// against a REAL Kernel.Run, through the same REST round trip
// rest_run_test.go already exercises — not just internal/obs's own
// isolated unit tests (internal/obs/tracer_test.go), which prove the
// span-nesting mechanism works but can't prove kernel/loop.go actually
// calls it in the right order/place. A spyTracer stands in for
// obs.OTLPExporter here for the same reason internal/provider/fake stands
// in for a live model: deterministic, in-process, no OTLP collector needed.
package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/authn"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// spySpan records what it was started with and, once closed, what it was
// ended with — enough to reconstruct the whole span tree a run produced.
type spySpan struct {
	name       string
	kind       obs.ObservationType
	parent     *spySpan // nil for a root span
	startAttrs obs.Attrs
	endAttrs   obs.Attrs
	input      string
	output     string
}

func (s *spySpan) End(attrs obs.Attrs) { s.endAttrs = attrs }

func (s *spySpan) SetContent(input, output string) { s.input, s.output = input, output }

type spyCtxKey struct{}

// spyTracer is obs.Tracer for a test: parent/child linking works exactly
// like OTLPExporter's real one (StartSpan reads whatever *spySpan the
// caller's ctx already carries and links to it, then stores itself under
// the same key for its own children) — proving kernel/loop.go's turnCtx
// threading actually produces a tree, not just a flat list.
type spyTracer struct {
	mu    sync.Mutex
	spans []*spySpan
}

func (t *spyTracer) StartSpan(ctx context.Context, name string, kind obs.ObservationType, attrs obs.Attrs) (context.Context, obs.Span) {
	parent, _ := ctx.Value(spyCtxKey{}).(*spySpan)
	s := &spySpan{name: name, kind: kind, parent: parent, startAttrs: attrs}
	t.mu.Lock()
	t.spans = append(t.spans, s)
	t.mu.Unlock()
	return context.WithValue(ctx, spyCtxKey{}, s), s
}

// Detach/Attach: reuses the same spyCtxKey StartSpan already keys off of —
// this spy has no real cross-process backend, but implementing these for
// real (rather than as no-ops) means a test using spyTracer through an
// actual goroutine boundary would nest correctly too, the same guarantee
// obs.OTLPExporter/obs.LangfuseExporter give internal/delegate/spawn.go.
func (t *spyTracer) Detach(ctx context.Context) obs.SpanLink {
	s, _ := ctx.Value(spyCtxKey{}).(*spySpan)
	if s == nil {
		return nil
	}
	return s
}

func (t *spyTracer) Attach(ctx context.Context, link obs.SpanLink) context.Context {
	if link == nil {
		return ctx
	}
	return context.WithValue(ctx, spyCtxKey{}, link)
}

func TestKernelTracerProducesRootGenerationToolTree(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID := uuid.New()
	userID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("generate KEK: %v", err)
	}
	keyStore := crypto.NewKeyStore(kek)

	// Turn 1: one tool call (exercises the tool span, nested under turn 1's
	// generation span). Turn 2: natural completion (a second, sibling
	// generation span under the same root — proves spans don't leak across
	// turns).
	fakeProvider := fake.New(
		fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "tool_use", ToolUseID: "tu1", ToolName: "demo_tool", Input: `{}`},
			{Kind: "done", Done: "stop"},
		}},
		fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "content", Text: "All done."},
			{Kind: "done", Done: "stop"},
		}},
	)

	tracer := &spyTracer{}
	starter := &testRunStarter{kernel: &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fakeProvider}),
		Tools:    successToolExecutor{},
		Budget:   kernel.NoopBudgetGate{},
		Store:    st,
		Tracer:   tracer,
	}}

	signingKey, err := authn.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate test signing key: %v", err)
	}
	issuer := authn.NewDevIssuer(signingKey)
	verifier := authn.NewDevVerifier(authn.PublicKey(signingKey))
	token := mustIssueToken(t, issuer, tenantID, userID)

	srv := rest.NewServer(starter, st, keyStore, nil)
	srv.Verifier = verifier
	srv.ControlPlane = newTestControlPlane(st)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	client := httpSrv.Client()
	createReq, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/runs", strings.NewReader(`{"input":"do the thing"}`))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createReq.Header.Set("content-type", "application/json")
	createResp, err := client.Do(createReq)
	if err != nil {
		t.Fatalf("POST /v1/runs: %v", err)
	}
	defer createResp.Body.Close() //nolint:errcheck // read-only response body
	if createResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/runs status = %d, want 202", createResp.StatusCode)
	}
	var created struct {
		RunID string `json:"run_id"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	// GET /v1/runs/{id}/events (SSE) reads until the server closes the
	// connection after writing the terminal event (rest_run_test.go's own
	// readSSEFrames doc comment) — the run has genuinely finished, and every
	// span it produced has already been recorded, by the time this returns.
	eventsReq, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/v1/runs/"+created.RunID+"/events", nil)
	eventsReq.Header.Set("Authorization", "Bearer "+token)
	eventsResp, err := client.Do(eventsReq)
	if err != nil {
		t.Fatalf("GET /v1/runs/{id}/events: %v", err)
	}
	defer eventsResp.Body.Close() //nolint:errcheck // read-only response body
	if eventsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET events status = %d, want 200", eventsResp.StatusCode)
	}
	if frames := readSSEFrames(t, bufio.NewReader(eventsResp.Body)); len(frames) == 0 {
		t.Fatal("no SSE frames received")
	}

	tracer.mu.Lock()
	spans := append([]*spySpan(nil), tracer.spans...)
	tracer.mu.Unlock()

	var roots, generations, tools []*spySpan
	for _, s := range spans {
		switch s.kind {
		case obs.ObservationAgent:
			roots = append(roots, s)
		case obs.ObservationGeneration:
			generations = append(generations, s)
		case obs.ObservationTool:
			tools = append(tools, s)
		}
	}
	if len(roots) != 1 {
		t.Fatalf("got %d root spans, want 1: %+v", len(roots), spans)
	}
	if len(generations) != 2 {
		t.Fatalf("got %d generation spans, want 2 (one per turn): %+v", len(generations), spans)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tool spans, want 1: %+v", len(tools), spans)
	}
	root := roots[0]
	if generations[0].parent != root || generations[1].parent != root {
		t.Error("both generation spans must be direct children of the root span")
	}
	if tools[0].parent != generations[0] {
		t.Error("the tool span must nest under turn 1's own generation span, not the root or turn 2's")
	}
	if root.endAttrs["terminal_reason"] != "completed" {
		t.Errorf("root span's terminal_reason = %q, want %q", root.endAttrs["terminal_reason"], "completed")
	}
	if tools[0].endAttrs["outcome"] != "ok" {
		t.Errorf("tool span's outcome = %q, want %q", tools[0].endAttrs["outcome"], "ok")
	}
	if generations[0].startAttrs["model.id"] == "" {
		t.Error("generation span missing model.id at start")
	}
	// Kernel.TraceContent was left at its zero value (false) for this whole
	// test — content must stay off unless explicitly requested.
	if generations[0].input != "" || generations[0].output != "" || tools[0].input != "" || tools[0].output != "" {
		t.Error("Kernel.TraceContent is false: no span should carry input/output content")
	}
}

// TestKernelTracerContentOptIn is the SetContent counterpart to the tree
// test above: with Kernel.TraceContent explicitly true, the generation and
// tool spans now carry the actual prompt/tool payload — proving the opt-in
// works, and (via the sibling test above, which never sets it) that leaving
// it unset keeps every span exactly as content-free as before this field
// existed.
func TestKernelTracerContentOptIn(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID := uuid.New()
	userID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("generate KEK: %v", err)
	}
	keyStore := crypto.NewKeyStore(kek)

	fakeProvider := fake.New(
		fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "tool_use", ToolUseID: "tu1", ToolName: "demo_tool", Input: `{"city":"nyc"}`},
			{Kind: "done", Done: "stop"},
		}},
		fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "content", Text: "All done."},
			{Kind: "done", Done: "stop"},
		}},
	)

	tracer := &spyTracer{}
	starter := &testRunStarter{kernel: &kernel.Kernel{
		Provider:     provider.Wrap([]provider.Provider{fakeProvider}),
		Tools:        successToolExecutor{},
		Budget:       kernel.NoopBudgetGate{},
		Store:        st,
		Tracer:       tracer,
		TraceContent: true,
	}}

	signingKey, err := authn.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate test signing key: %v", err)
	}
	issuer := authn.NewDevIssuer(signingKey)
	verifier := authn.NewDevVerifier(authn.PublicKey(signingKey))
	token := mustIssueToken(t, issuer, tenantID, userID)

	srv := rest.NewServer(starter, st, keyStore, nil)
	srv.Verifier = verifier
	srv.ControlPlane = newTestControlPlane(st)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	client := httpSrv.Client()
	createReq, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/runs", strings.NewReader(`{"input":"what's the weather in nyc"}`))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createReq.Header.Set("content-type", "application/json")
	createResp, err := client.Do(createReq)
	if err != nil {
		t.Fatalf("POST /v1/runs: %v", err)
	}
	defer createResp.Body.Close() //nolint:errcheck // read-only response body
	if createResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/runs status = %d, want 202", createResp.StatusCode)
	}
	var created struct {
		RunID string `json:"run_id"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	eventsReq, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/v1/runs/"+created.RunID+"/events", nil)
	eventsReq.Header.Set("Authorization", "Bearer "+token)
	eventsResp, err := client.Do(eventsReq)
	if err != nil {
		t.Fatalf("GET /v1/runs/{id}/events: %v", err)
	}
	defer eventsResp.Body.Close() //nolint:errcheck // read-only response body
	if frames := readSSEFrames(t, bufio.NewReader(eventsResp.Body)); len(frames) == 0 {
		t.Fatal("no SSE frames received")
	}

	tracer.mu.Lock()
	spans := append([]*spySpan(nil), tracer.spans...)
	tracer.mu.Unlock()

	var generation, tool *spySpan
	for _, s := range spans {
		switch s.kind { //nolint:exhaustive // this test only cares about the generation/tool spans' content; the root (agent) span carries none
		case obs.ObservationGeneration:
			if generation == nil { // the first turn's — the one that requested the tool call
				generation = s
			}
		case obs.ObservationTool:
			tool = s
		}
	}
	if generation == nil || tool == nil {
		t.Fatalf("missing generation or tool span: %+v", spans)
	}

	if !strings.Contains(generation.input, "what's the weather in nyc") {
		t.Errorf("generation span input missing the user's message: %q", generation.input)
	}
	if !strings.Contains(generation.output, "demo_tool") {
		t.Errorf("generation span output missing the requested tool call: %q", generation.output)
	}
	if tool.input != `{"city":"nyc"}` {
		t.Errorf("tool span input = %q, want the tool's actual arguments", tool.input)
	}
	if tool.output != "{}" {
		t.Errorf("tool span output = %q, want the tool's actual result", tool.output)
	}
}

// successToolExecutor always succeeds — unlike
// kernel.NotImplementedToolExecutor (rest_run_test.go's own choice), this
// test needs a REAL successful dispatch so the tool span's End sees
// outcome=ok, not a synthetic denial.
type successToolExecutor struct{}

func (successToolExecutor) Execute(_ context.Context, _ kernel.ToolUseRequest, _ kernel.ExecContext) kernel.ToolResult {
	return kernel.ToolResult{Output: []byte(`{}`)}
}
