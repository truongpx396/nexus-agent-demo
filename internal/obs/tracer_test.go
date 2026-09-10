package obs

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func attrMap(kvs []tracetest.SpanStub) map[string]map[string]string {
	out := make(map[string]map[string]string, len(kvs))
	for _, s := range kvs {
		m := make(map[string]string, len(s.Attributes))
		for _, kv := range s.Attributes {
			// String(), not AsString(): AsString() only reads a
			// STRING-typed value — gen_ai.usage.*_tokens are
			// attribute.Int64, which String() formats uniformly regardless
			// of type.
			m[string(kv.Key)] = kv.Value.String()
		}
		out[s.Name] = m
	}
	return out
}

// TestOTLPExporter_StartSpanNestsUnderParent proves the ONE property that
// makes a Langfuse trace read as an agent graph rather than a flat list of
// unrelated spans (docs/local-llm.md): a child span started with the ctx
// StartSpan returns carries the parent's real OTel SpanID/TraceID — plain
// standard OTel parent/child propagation, no manual trace-ID plumbing.
func TestOTLPExporter_StartSpanNestsUnderParent(t *testing.T) {
	recorder := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(recorder))
	exp := &OTLPExporter{tracer: tp.Tracer("test")}

	rootCtx, rootSpan := exp.StartSpan(context.Background(), "kernel.run", ObservationAgent, Attrs{"session.id": "s-1"})
	childCtx, childSpan := exp.StartSpan(rootCtx, "model.call", ObservationGeneration, Attrs{"model.id": "m-1"})
	_, grandchild := exp.StartSpan(childCtx, "tool.call", ObservationTool, Attrs{"tool.id": "t-1"})
	grandchild.End(Attrs{"outcome": "ok"})
	childSpan.End(Attrs{"usage.output": "12"})
	rootSpan.End(Attrs{"terminal_reason": "completed"})

	spans := recorder.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 3: %+v", len(spans), spans)
	}
	byName := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	root, ok := byName["kernel.run"]
	if !ok {
		t.Fatal("missing root span kernel.run")
	}
	generation, ok := byName["model.call"]
	if !ok {
		t.Fatal("missing generation span model.call")
	}
	tool, ok := byName["tool.call"]
	if !ok {
		t.Fatal("missing tool span tool.call")
	}
	if generation.Parent.SpanID() != root.SpanContext.SpanID() {
		t.Errorf("generation span's parent = %s, want root's span id %s", generation.Parent.SpanID(), root.SpanContext.SpanID())
	}
	if tool.Parent.SpanID() != generation.SpanContext.SpanID() {
		t.Errorf("tool span's parent = %s, want generation's span id %s", tool.Parent.SpanID(), generation.SpanContext.SpanID())
	}
	if root.SpanContext.TraceID() != generation.SpanContext.TraceID() || root.SpanContext.TraceID() != tool.SpanContext.TraceID() {
		t.Error("root/generation/tool spans do not share one trace id")
	}
}

// TestOTLPExporter_StartSpanSetsLangfuseObservationType proves the
// attribute Langfuse's OTel ingestion actually keys its trace-view
// rendering on (docs/local-llm.md) is set correctly per ObservationType,
// and that a non-generation span carries no gen_ai.* alias (there is no
// usage to alias for a tool/agent span).
func TestOTLPExporter_StartSpanSetsLangfuseObservationType(t *testing.T) {
	recorder := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(recorder))
	exp := &OTLPExporter{tracer: tp.Tracer("test")}

	_, toolSpan := exp.StartSpan(context.Background(), "tool.call", ObservationTool, Attrs{"tool.id": "t-1"})
	toolSpan.End(nil)

	attrs := attrMap(recorder.GetSpans())["tool.call"]
	if attrs["langfuse.observation.type"] != "tool" {
		t.Errorf("langfuse.observation.type = %q, want %q", attrs["langfuse.observation.type"], "tool")
	}
	if _, present := attrs["gen_ai.request.model"]; present {
		t.Error("a tool span must not carry a gen_ai.* alias")
	}
}

// TestOTLPExporter_GenerationSpanAliasesGenAIAttributes proves the
// generation-only translation: model.id/usage.* (already-allowlisted keys)
// get GenAI semantic-convention ALIASES added alongside them — never
// instead of them, and never a prompt/completion (constitution Principle
// VI stays untouched by this path).
func TestOTLPExporter_GenerationSpanAliasesGenAIAttributes(t *testing.T) {
	recorder := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(recorder))
	exp := &OTLPExporter{tracer: tp.Tracer("test")}

	_, span := exp.StartSpan(context.Background(), "model.call", ObservationGeneration, Attrs{"model.id": "qwen2.5-local"})
	span.End(Attrs{
		"usage.input_uncached":   "40",
		"usage.input_cache_read": "10",
		"usage.output":           "12",
		"outcome":                "stop",
	})

	attrs := attrMap(recorder.GetSpans())["model.call"]
	if attrs["model.id"] != "qwen2.5-local" || attrs["gen_ai.request.model"] != "qwen2.5-local" {
		t.Errorf("model.id/gen_ai.request.model = %q/%q", attrs["model.id"], attrs["gen_ai.request.model"])
	}
	if attrs["gen_ai.usage.input_tokens"] != "50" { // 40 uncached + 10 cache-read
		t.Errorf("gen_ai.usage.input_tokens = %q, want %q", attrs["gen_ai.usage.input_tokens"], "50")
	}
	if attrs["gen_ai.usage.output_tokens"] != "12" {
		t.Errorf("gen_ai.usage.output_tokens = %q, want %q", attrs["gen_ai.usage.output_tokens"], "12")
	}
	if _, present := attrs["gen_ai.prompt"]; present {
		t.Error("gen_ai.prompt must never be set — content stays out of this path")
	}
	if _, present := attrs["gen_ai.completion"]; present {
		t.Error("gen_ai.completion must never be set — content stays out of this path")
	}
}

// TestOTLPExporter_StartSpanFiltersNonAllowlistedAttrs proves StartSpan/End
// go through the same deny-by-default allowlist Emit already does — the
// Tracer path is not a way around Principle VI.
func TestOTLPExporter_StartSpanFiltersNonAllowlistedAttrs(t *testing.T) {
	recorder := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(recorder))
	exp := &OTLPExporter{tracer: tp.Tracer("test")}

	_, span := exp.StartSpan(context.Background(), "kernel.run", ObservationAgent, Attrs{
		"session.id":   "s-1",
		"user_message": "this must never appear",
	})
	span.End(Attrs{"secret": "nope"})

	attrs := attrMap(recorder.GetSpans())["kernel.run"]
	if attrs["session.id"] != "s-1" {
		t.Errorf("allowlisted session.id missing: %v", attrs)
	}
	if _, present := attrs["user_message"]; present {
		t.Error("a non-allowlisted attribute reached the span via StartSpan")
	}
	if _, present := attrs["secret"]; present {
		t.Error("a non-allowlisted attribute reached the span via End")
	}
}
