package obs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// captureServer records every batch POSTed to /api/public/ingestion,
// decoding each event's body into a map so a test can assert on whichever
// fields it cares about without needing the exporter's own unexported
// body structs.
type captureServer struct {
	t      *testing.T
	server *httptest.Server

	mu     sync.Mutex
	events []capturedEvent
	auth   struct{ user, pass string }
}

type capturedEvent struct {
	Type string
	Body map[string]any
}

func newCaptureServer(t *testing.T) *captureServer {
	t.Helper()
	cs := &captureServer{t: t}
	cs.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		var req struct {
			Batch []struct {
				Type string         `json:"type"`
				Body map[string]any `json:"body"`
			} `json:"batch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode ingestion batch: %v", err)
		}
		cs.mu.Lock()
		cs.auth.user, cs.auth.pass = user, pass
		for _, e := range req.Batch {
			cs.events = append(cs.events, capturedEvent{Type: e.Type, Body: e.Body})
		}
		cs.mu.Unlock()
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`{"successes":[],"errors":[]}`))
	}))
	t.Cleanup(cs.server.Close)
	return cs
}

// waitForEvents polls until at least n events have been captured, or fails
// the test — the exporter flushes on its own background ticker, so a test
// can't just read cs.events immediately after calling StartSpan/End.
func (cs *captureServer) waitForEvents(n int) []capturedEvent {
	cs.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cs.mu.Lock()
		got := len(cs.events)
		cs.mu.Unlock()
		if got >= n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]capturedEvent, len(cs.events))
	copy(out, cs.events)
	return out
}

func newTestLangfuseExporter(t *testing.T, cs *captureServer) *LangfuseExporter {
	t.Helper()
	e := newLangfuseExporterWithInterval(cs.server.URL, "pk-test", "sk-test", 20*time.Millisecond)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	return e
}

func eventsByType(events []capturedEvent, typ string) []capturedEvent {
	var out []capturedEvent
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestLangfuseExporter_StartSpanNestsUnderParent(t *testing.T) {
	cs := newCaptureServer(t)
	exp := newTestLangfuseExporter(t, cs)

	rootCtx, rootSpan := exp.StartSpan(context.Background(), "kernel.run", ObservationAgent, Attrs{"session.id": "s-1"})
	childCtx, childSpan := exp.StartSpan(rootCtx, "model.call", ObservationGeneration, Attrs{"model.id": "m-1"})
	_, grandchild := exp.StartSpan(childCtx, "tool.call", ObservationTool, Attrs{"tool.id": "t-1"})
	grandchild.End(Attrs{"outcome": "ok"})
	childSpan.End(Attrs{"usage.output": "12"})
	rootSpan.End(Attrs{"terminal_reason": "completed"})

	events := cs.waitForEvents(5) // trace-create + 3 creates + ... (updates may still be flushing)
	traces := eventsByType(events, "trace-create")
	if len(traces) != 1 {
		t.Fatalf("got %d trace-create events, want 1: %+v", len(traces), events)
	}
	traceID, _ := traces[0].Body["id"].(string)
	if traceID == "" {
		t.Fatal("trace-create body has no id")
	}

	spanCreates := eventsByType(events, "span-create")
	genCreates := eventsByType(events, "generation-create")
	if len(spanCreates) != 2 { // root (agent) + tool
		t.Fatalf("got %d span-create events, want 2: %+v", len(spanCreates), events)
	}
	if len(genCreates) != 1 {
		t.Fatalf("got %d generation-create events, want 1: %+v", len(genCreates), events)
	}

	var root, tool map[string]any
	for _, s := range spanCreates {
		if s.Body["name"] == "kernel.run" {
			root = s.Body
		}
		if s.Body["name"] == "tool.call" {
			tool = s.Body
		}
	}
	generation := genCreates[0].Body

	if root == nil || tool == nil {
		t.Fatalf("missing root or tool span-create: %+v", events)
	}
	if root["traceId"] != traceID || generation["traceId"] != traceID || tool["traceId"] != traceID {
		t.Errorf("not all observations share the trace id %q: root=%v generation=%v tool=%v", traceID, root["traceId"], generation["traceId"], tool["traceId"])
	}
	if root["parentObservationId"] != nil && root["parentObservationId"] != "" {
		t.Errorf("root observation should have no parent, got %v", root["parentObservationId"])
	}
	rootID, _ := root["id"].(string)
	genID, _ := generation["id"].(string)
	if generation["parentObservationId"] != rootID {
		t.Errorf("generation's parentObservationId = %v, want root id %v", generation["parentObservationId"], rootID)
	}
	if tool["parentObservationId"] != genID {
		t.Errorf("tool's parentObservationId = %v, want generation id %v", tool["parentObservationId"], genID)
	}
}

func TestLangfuseExporter_StartSpanFiltersNonAllowlistedAttrs(t *testing.T) {
	cs := newCaptureServer(t)
	exp := newTestLangfuseExporter(t, cs)

	_, span := exp.StartSpan(context.Background(), "kernel.run", ObservationAgent, Attrs{
		"session.id":   "s-1",
		"user_message": "this must never appear",
	})
	span.End(Attrs{"secret": "nope"})

	events := cs.waitForEvents(2)
	for _, e := range events {
		meta, _ := e.Body["metadata"].(map[string]any)
		if _, present := meta["user_message"]; present {
			t.Errorf("non-allowlisted attribute user_message reached %s metadata: %+v", e.Type, meta)
		}
		if _, present := meta["secret"]; present {
			t.Errorf("non-allowlisted attribute secret reached %s metadata: %+v", e.Type, meta)
		}
	}
	create := eventsByType(events, "span-create")[0]
	if meta, _ := create.Body["metadata"].(map[string]any); meta["session.id"] != "s-1" {
		t.Errorf("allowlisted session.id missing from span-create metadata: %+v", meta)
	}
}

func TestLangfuseExporter_GenerationEventPopulatesModelAndUsage(t *testing.T) {
	cs := newCaptureServer(t)
	exp := newTestLangfuseExporter(t, cs)

	_, span := exp.StartSpan(context.Background(), "model.call", ObservationGeneration, Attrs{"model.id": "qwen2.5-local"})
	span.End(Attrs{
		"usage.input_uncached":   "40",
		"usage.input_cache_read": "10",
		"usage.output":           "12",
	})

	events := cs.waitForEvents(2)
	create := eventsByType(events, "generation-create")[0]
	if create.Body["model"] != "qwen2.5-local" {
		t.Errorf("generation-create model = %v, want qwen2.5-local", create.Body["model"])
	}

	update := eventsByType(events, "generation-update")[0]
	if update.Body["model"] != "qwen2.5-local" {
		t.Errorf("generation-update model = %v, want qwen2.5-local", update.Body["model"])
	}
	usage, ok := update.Body["usage"].(map[string]any)
	if !ok {
		t.Fatalf("generation-update has no usage object: %+v", update.Body)
	}
	if usage["input"] != float64(50) { // 40 uncached + 10 cache-read
		t.Errorf("usage.input = %v, want 50", usage["input"])
	}
	if usage["output"] != float64(12) {
		t.Errorf("usage.output = %v, want 12", usage["output"])
	}

	// A non-generation span must carry neither field.
	_, toolSpan := exp.StartSpan(context.Background(), "tool.call", ObservationTool, Attrs{"tool.id": "t-1"})
	toolSpan.End(nil)
	events = cs.waitForEvents(len(events) + 2)
	for _, e := range eventsByType(events, "span-create") {
		if e.Body["name"] == "tool.call" {
			if _, present := e.Body["model"]; present {
				t.Error("a tool span-create must not carry a model field")
			}
			if _, present := e.Body["usage"]; present {
				t.Error("a tool span-create must not carry a usage field")
			}
		}
	}
}

func TestLangfuseExporter_SetContentOnlyWhenNonEmpty(t *testing.T) {
	cs := newCaptureServer(t)
	exp := newTestLangfuseExporter(t, cs)

	_, withContent := exp.StartSpan(context.Background(), "model.call", ObservationGeneration, Attrs{"model.id": "m-1"})
	withContent.SetContent("the prompt", "the completion")
	withContent.End(nil)

	events := cs.waitForEvents(2)
	update := eventsByType(events, "generation-update")[0]
	if update.Body["input"] != "the prompt" || update.Body["output"] != "the completion" {
		t.Errorf("SetContent input/output missing from generation-update: %+v", update.Body)
	}

	// A second, separate span that never calls SetContent must carry
	// neither field at all (omitempty should drop them, not send empty
	// strings) — a fresh trace root so it has no other generation-update
	// to be confused with.
	_, noContent := exp.StartSpan(context.Background(), "model.call", ObservationGeneration, Attrs{"model.id": "m-2"})
	noContent.End(nil)
	events = cs.waitForEvents(4)
	for _, e := range eventsByType(events, "generation-update") {
		if e.Body["model"] != "m-2" {
			continue
		}
		if _, present := e.Body["input"]; present {
			t.Errorf("generation-update without SetContent must have no input field: %+v", e.Body)
		}
		if _, present := e.Body["output"]; present {
			t.Errorf("generation-update without SetContent must have no output field: %+v", e.Body)
		}
	}
}

// TestLangfuseExporter_NestsAcrossGoroutineBoundaryLikeDelegateSpawn proves
// the property internal/delegate/spawn.go's own Spawn actually depends on:
// a delegated subagent's root span, started from a FRESH context.Background()
// on a different goroutine that only carries the parent's re-injected
// trace.SpanContext (exactly spawn.go's own
// `trace.ContextWithRemoteSpanContext(bg, parentSpanCtx)` pattern, copied
// verbatim here), must still nest under the parent trace rather than
// opening as a second, disconnected one. An earlier version of this
// exporter used a private ctx-key for propagation instead of
// trace.SpanContext and failed this exact scenario (parentSpanCtx.IsValid()
// was false across the boundary) — this test is what would have caught it.
func TestLangfuseExporter_NestsAcrossGoroutineBoundaryLikeDelegateSpawn(t *testing.T) {
	cs := newCaptureServer(t)
	exp := newTestLangfuseExporter(t, cs)

	parentCtx, parentSpan := exp.StartSpan(context.Background(), "kernel.run", ObservationAgent, Attrs{"session.id": "parent-1"})

	// internal/delegate/spawn.go, verbatim:
	parentSpanCtx := trace.SpanContextFromContext(parentCtx)
	bg := context.Background()
	if parentSpanCtx.IsValid() {
		bg = trace.ContextWithRemoteSpanContext(bg, parentSpanCtx)
	}
	if !parentSpanCtx.IsValid() {
		t.Fatal("StartSpan's returned ctx must carry a valid trace.SpanContext — spawn.go's cross-goroutine propagation depends on it")
	}

	_, childSpan := exp.StartSpan(bg, "kernel.run", ObservationAgent, Attrs{"session.id": "child-1"})
	childSpan.End(nil)
	parentSpan.End(nil)

	events := cs.waitForEvents(2)
	traces := eventsByType(events, "trace-create")
	if len(traces) != 1 {
		t.Fatalf("got %d trace-create events, want 1 — the delegated child opened a disconnected new trace instead of nesting: %+v", len(traces), traces)
	}

	spanCreates := eventsByType(events, "span-create")
	if len(spanCreates) != 2 {
		t.Fatalf("got %d span-create events, want 2: %+v", len(spanCreates), spanCreates)
	}
	var parent, child map[string]any
	for _, s := range spanCreates {
		switch s.Body["metadata"].(map[string]any)["session.id"] {
		case "parent-1":
			parent = s.Body
		case "child-1":
			child = s.Body
		}
	}
	if parent == nil || child == nil {
		t.Fatalf("missing parent or child span-create: %+v", spanCreates)
	}
	if child["traceId"] != parent["traceId"] {
		t.Errorf("child traceId = %v, want parent's traceId %v", child["traceId"], parent["traceId"])
	}
	if child["parentObservationId"] != parent["id"] {
		t.Errorf("child parentObservationId = %v, want parent's id %v", child["parentObservationId"], parent["id"])
	}
}

func TestLangfuseExporter_EmitSendsEventCreateWithoutTraceID(t *testing.T) {
	cs := newCaptureServer(t)
	exp := newTestLangfuseExporter(t, cs)

	if err := exp.Emit("webhook.received", Attrs{"session.id": "s-1"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	events := cs.waitForEvents(1)
	create := eventsByType(events, "event-create")
	if len(create) != 1 {
		t.Fatalf("got %d event-create events, want 1: %+v", len(create), events)
	}
	if _, present := create[0].Body["traceId"]; present {
		t.Errorf("Emit's event-create should carry no traceId, got %v", create[0].Body["traceId"])
	}
}

func TestLangfuseExporter_UsesBasicAuth(t *testing.T) {
	cs := newCaptureServer(t)
	exp := newTestLangfuseExporter(t, cs)

	_ = exp.Emit("x", Attrs{"session.id": "s-1"})
	cs.waitForEvents(1)

	cs.mu.Lock()
	user, pass := cs.auth.user, cs.auth.pass
	cs.mu.Unlock()
	if user != "pk-test" || pass != "sk-test" {
		t.Errorf("basic auth = %q:%q, want pk-test:sk-test", user, pass)
	}
}

func TestLangfuseExporter_ShutdownFlushesPending(t *testing.T) {
	cs := newCaptureServer(t)
	// A long flush interval: only Shutdown's own final flush should deliver
	// this event within the test's lifetime.
	exp := newLangfuseExporterWithInterval(cs.server.URL, "pk-test", "sk-test", time.Hour)

	_ = exp.Emit("x", Attrs{"session.id": "s-1"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := exp.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	cs.mu.Lock()
	got := len(cs.events)
	cs.mu.Unlock()
	if got != 1 {
		t.Errorf("got %d events after Shutdown, want 1 (Shutdown should flush synchronously)", got)
	}
}
