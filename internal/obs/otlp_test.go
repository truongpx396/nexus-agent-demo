package obs

import (
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newTestOTLPExporter builds an OTLPExporter around an in-memory span
// recorder instead of a real network dial — proves Emit's own filtering and
// span-shape behavior (README task 13.12, closing production-readiness
// finding F13) without needing a live OTLP collector.
func newTestOTLPExporter(t *testing.T) (*OTLPExporter, *tracetest.InMemoryExporter) {
	t.Helper()
	recorder := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(recorder))
	return &OTLPExporter{tracer: tp.Tracer("test"), tp: tp}, recorder
}

func TestOTLPExporter_EmitFiltersAttributes(t *testing.T) {
	exp, recorder := newTestOTLPExporter(t)

	if err := exp.Emit("turn.completed", Attrs{
		"session.id":      "abc-123",
		"terminal_reason": "completed",
		"user_message":    "this is conversation content and must never appear",
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	spans := recorder.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "turn.completed" {
		t.Errorf("span name = %q, want %q", span.Name, "turn.completed")
	}

	attrs := map[string]string{}
	for _, kv := range span.Attributes {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["session.id"] != "abc-123" || attrs["terminal_reason"] != "completed" {
		t.Errorf("allowed attributes missing or wrong: %v", attrs)
	}
	if _, present := attrs["user_message"]; present {
		t.Error("Emit let a non-allowlisted attribute (user_message) reach the span")
	}
}

func TestOTLPExporter_EmitTracksDrops(t *testing.T) {
	exp, _ := newTestOTLPExporter(t)
	tracker := &DropTracker{}
	exp.Drops = tracker

	if err := exp.Emit("turn.completed", Attrs{
		"session.id": "abc-123", // allowed
		"secret":     "nope",    // dropped
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if tracker.Rate() <= 0 {
		t.Errorf("DropTracker.Rate() = %v, want > 0 after a filtered attribute", tracker.Rate())
	}
}
