package obs

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// fakeSpanTarget stands in for a real destination (OTLPExporter or
// LangfuseExporter) using the EXACT SAME trace.SpanContext propagation
// mechanism both of them actually use — the shared-ctx-slot bug
// MultiExporter exists to avoid (multi.go's own doc comment) is
// specifically about that mechanism, so faking it any other way (e.g. a
// private per-instance ctx key) wouldn't exercise the thing that matters.
type fakeSpanTarget struct {
	mu            sync.Mutex
	events        []fakeEvent
	emitErr       error
	shutdownErr   error
	shutdownCalls int
}

type fakeEvent struct {
	name      string
	traceID   trace.TraceID
	spanID    trace.SpanID
	parentID  trace.SpanID
	hasParent bool
	input     string
	output    string
}

func (f *fakeSpanTarget) StartSpan(ctx context.Context, name string, kind ObservationType, attrs Attrs) (context.Context, Span) {
	parent := trace.SpanContextFromContext(ctx)
	var traceID trace.TraceID
	if parent.IsValid() {
		traceID = parent.TraceID()
	} else {
		_, _ = rand.Read(traceID[:])
	}
	var spanID trace.SpanID
	_, _ = rand.Read(spanID[:])

	f.mu.Lock()
	idx := len(f.events)
	ev := fakeEvent{name: name, traceID: traceID, spanID: spanID}
	if parent.IsValid() {
		ev.parentID = parent.SpanID()
		ev.hasParent = true
	}
	f.events = append(f.events, ev)
	f.mu.Unlock()

	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	return trace.ContextWithSpanContext(ctx, sc), &fakeSpan{target: f, index: idx}
}

func (f *fakeSpanTarget) Detach(ctx context.Context) SpanLink {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return sc
}

func (f *fakeSpanTarget) Attach(ctx context.Context, link SpanLink) context.Context {
	sc, ok := link.(trace.SpanContext)
	if !ok {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}

func (f *fakeSpanTarget) Emit(name string, attrs Attrs) error {
	f.mu.Lock()
	f.events = append(f.events, fakeEvent{name: name})
	f.mu.Unlock()
	return f.emitErr
}

func (f *fakeSpanTarget) Shutdown(context.Context) error {
	f.mu.Lock()
	f.shutdownCalls++
	f.mu.Unlock()
	return f.shutdownErr
}

func (f *fakeSpanTarget) eventsSnapshot() []fakeEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeEvent, len(f.events))
	copy(out, f.events)
	return out
}

type fakeSpan struct {
	target *fakeSpanTarget
	index  int
}

func (s *fakeSpan) End(Attrs) {}

func (s *fakeSpan) SetContent(input, output string) {
	s.target.mu.Lock()
	s.target.events[s.index].input = input
	s.target.events[s.index].output = output
	s.target.mu.Unlock()
}

func TestMultiExporter_BothTargetsReceiveIndependentlyNestedSpans(t *testing.T) {
	a, b := &fakeSpanTarget{}, &fakeSpanTarget{}
	m := NewMultiExporter(a, b)

	rootCtx, rootSpan := m.StartSpan(context.Background(), "kernel.run", ObservationAgent, Attrs{"session.id": "s-1"})
	_, childSpan := m.StartSpan(rootCtx, "model.call", ObservationGeneration, Attrs{"model.id": "m-1"})
	childSpan.End(nil)
	rootSpan.End(nil)

	for name, f := range map[string]*fakeSpanTarget{"a": a, "b": b} {
		events := f.eventsSnapshot()
		if len(events) != 2 {
			t.Fatalf("target %s: got %d events, want 2", name, len(events))
		}
		root, child := events[0], events[1]
		if child.traceID != root.traceID {
			t.Errorf("target %s: child traceID != root traceID", name)
		}
		if !child.hasParent || child.parentID != root.spanID {
			t.Errorf("target %s: child parentID = %v (hasParent=%v), want root spanID %v", name, child.parentID, child.hasParent, root.spanID)
		}
	}

	// The two targets must NOT share ids — each mints its own, independent
	// of the other, proving no cross-contamination through a shared ctx
	// slot.
	if a.events[0].traceID == b.events[0].traceID {
		t.Error("target a and target b share a traceID — they should be completely independent")
	}
}

// TestMultiExporter_NestsAcrossSimulatedGoroutineBoundary mirrors
// internal/obs/langfuse_test.go's own
// TestLangfuseExporter_NestsAcrossGoroutineBoundaryLikeDelegateSpawn,
// copying internal/delegate/spawn.go's real Detach/Attach pattern — this
// is the test that would have caught the naive-tee bug (both targets
// reading/writing ONE shared trace.SpanContext slot, so whichever ran
// second would corrupt the other's later nesting) if MultiExporter had
// been built that way instead of giving each target its own ctx thread.
func TestMultiExporter_NestsAcrossSimulatedGoroutineBoundary(t *testing.T) {
	a, b := &fakeSpanTarget{}, &fakeSpanTarget{}
	m := NewMultiExporter(a, b)

	parentCtx, parentSpan := m.StartSpan(context.Background(), "kernel.run", ObservationAgent, Attrs{"session.id": "parent-1"})

	// internal/delegate/spawn.go's real pattern, generalized:
	link := m.Detach(parentCtx)
	bg := context.Background()
	bg = m.Attach(bg, link)

	_, childSpan := m.StartSpan(bg, "kernel.run", ObservationAgent, Attrs{"session.id": "child-1"})
	childSpan.End(nil)
	parentSpan.End(nil)

	for name, f := range map[string]*fakeSpanTarget{"a": a, "b": b} {
		events := f.eventsSnapshot()
		if len(events) != 2 {
			t.Fatalf("target %s: got %d events, want 2", name, len(events))
		}
		parent, child := events[0], events[1]
		if child.traceID != parent.traceID {
			t.Errorf("target %s: child traceID != parent traceID — opened a disconnected new trace instead of nesting", name)
		}
		if !child.hasParent || child.parentID != parent.spanID {
			t.Errorf("target %s: child parentID = %v (hasParent=%v), want parent spanID %v", name, child.parentID, child.hasParent, parent.spanID)
		}
	}
}

func TestMultiExporter_EmitFansOutAndToleratesOneFailure(t *testing.T) {
	a := &fakeSpanTarget{}
	b := &fakeSpanTarget{emitErr: errors.New("b unreachable")}
	m := NewMultiExporter(a, b)

	err := m.Emit("run.terminal", Attrs{"session.id": "s-1"})
	if err == nil {
		t.Fatal("Emit should return a combined error when one target fails")
	}
	if len(a.eventsSnapshot()) != 1 {
		t.Error("target a should still receive Emit even though target b fails")
	}
	if len(b.eventsSnapshot()) != 1 {
		t.Error("target b should still be called (and recorded) even though it errors")
	}
}

func TestMultiExporter_SetContentFansOutToBothSpans(t *testing.T) {
	a, b := &fakeSpanTarget{}, &fakeSpanTarget{}
	m := NewMultiExporter(a, b)

	_, span := m.StartSpan(context.Background(), "model.call", ObservationGeneration, Attrs{"model.id": "m-1"})
	span.SetContent("the prompt", "the completion")
	span.End(nil)

	for name, f := range map[string]*fakeSpanTarget{"a": a, "b": b} {
		events := f.eventsSnapshot()
		if len(events) != 1 || events[0].input != "the prompt" || events[0].output != "the completion" {
			t.Errorf("target %s: SetContent did not reach it: %+v", name, events)
		}
	}
}

func TestMultiExporter_ShutdownFansOutAndToleratesOneFailure(t *testing.T) {
	a := &fakeSpanTarget{}
	b := &fakeSpanTarget{shutdownErr: errors.New("b shutdown failed")}
	// A third target with no Shutdown method at all (the stdout Exporter)
	// must be tolerated, not panic.
	c := NewExporter(io.Discard)
	m := NewMultiExporter(a, b, c)

	err := m.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown should return a combined error when one target fails")
	}
	if a.shutdownCalls != 1 {
		t.Errorf("target a Shutdown calls = %d, want 1", a.shutdownCalls)
	}
	if b.shutdownCalls != 1 {
		t.Errorf("target b Shutdown calls = %d, want 1", b.shutdownCalls)
	}
}
