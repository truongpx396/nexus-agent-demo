package obs

import "context"

// ObservationType classifies a span the way Langfuse's OTel ingestion reads
// it back out (docs/local-llm.md): an explicit `langfuse.observation.type`
// attribute is Langfuse's own highest-priority signal for how to render a
// span (plain step, model call, or tool call) in its trace view — set only
// at the OTLPExporter export boundary (otlp.go), never part of the
// allowlisted Attrs vocabulary itself.
type ObservationType string

const (
	// ObservationAgent is the one root span kernel.Kernel opens per
	// Run/Resume/ResumeDelegation/Continue call — the trace's own container.
	ObservationAgent ObservationType = "agent"
	// ObservationGeneration is one model call (kernel.Kernel.runTurns' and
	// .condense's own Provider.Stream call).
	ObservationGeneration ObservationType = "generation"
	// ObservationTool is one dispatched tool_use (kernel.Kernel.runTurns'
	// Tools.Execute call).
	ObservationTool ObservationType = "tool"
)

// Tracer is the span-nesting counterpart to Emitter's fire-and-forget point
// events (span.go) — kernel/loop.go needs real start/end pairs with parent
// propagation to give an agent's run genuine per-step visibility (one trace
// per run, a generation node per model call, a tool node per dispatched
// call), the feature span.go's own doc comment already earmarked as
// "Phase 6/9" and never built. Nil is valid on Kernel.Tracer, the same
// "nil means this control isn't wired" convention Kernel.Receipts/OnSuspend/
// Stuck already use — every caller goes through Kernel's own startSpan
// helper, which nil-checks once so call sites never do it themselves.
type Tracer interface {
	// StartSpan opens a span as a child of whatever span (if any) ctx
	// already carries, and returns a ctx carrying THIS span — a caller that
	// starts a child span with the returned ctx gets correct nesting for
	// free, standard OTel parent/child propagation, no manual trace-ID
	// plumbing.
	StartSpan(ctx context.Context, name string, kind ObservationType, attrs Attrs) (context.Context, Span)
}

// Span is one open span returned by Tracer.StartSpan.
type Span interface {
	// End closes the span, merging attrs known only at close time (usage,
	// outcome, terminal_reason) with whatever StartSpan already set — same
	// allowlist-filtering guarantee as Emit, applied at End as well as at
	// StartSpan.
	End(attrs Attrs)

	// SetContent attaches this span's raw input/output payload — a model
	// call's prompt/completion, or a tool call's input/result — as
	// Langfuse's own universal `langfuse.observation.input`/`.output`
	// attributes (docs/local-llm.md; confirmed against Langfuse's own OTel
	// ingestion docs as working across every observation type, not just
	// generations). Call it BEFORE End, same as OTel's own
	// Span.SetAttributes-before-End convention — a real span attribute set
	// after End is dropped.
	//
	// This is deliberately NOT routed through Filter/the allowlist every
	// other attribute on this type goes through: content is exactly what
	// Filter exists to keep out (constitution Principle VI — "Content is
	// reachable only through the event log ... never through telemetry").
	// It does not loosen that promise; it is a SEPARATE, explicitly-opt-in
	// channel a caller reaches only when it has independently decided
	// content egress is authorized (kernel.Kernel.TraceContent /
	// NEXUS_TRACE_CONTENT=true, docs/local-llm.md) — every call site that
	// hasn't made that decision simply never calls this method, and a
	// Tracer that never receives a SetContent call behaves exactly as it
	// did before this method existed.
	SetContent(input, output string)
}
