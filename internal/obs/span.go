package obs

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

// spanLine is the minimal shape the exporter below emits as one line of
// newline-delimited JSON — the stdout stand-in for the OTLP exporter's real
// spans (otlp.go). Kind/DurationMS are populated only via the Tracer path
// (StartSpan/End below); Emit's plain point-event call sites leave them
// zero, which `omitempty` drops from the encoded line so that older output
// stays byte-identical.
type spanLine struct {
	Name       string          `json:"name"`
	Attrs      Attrs           `json:"attrs"`
	Kind       ObservationType `json:"kind,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	// Input/Output are populated only via SetContent below, which only a
	// caller that has opted into content egress ever calls (Span.SetContent's
	// own doc comment) — every span before that opt-in leaves these unset,
	// which `omitempty` drops from the encoded line.
	Input  string `json:"input,omitempty"`
	Output string `json:"output,omitempty"`
}

// Exporter writes filtered spans as newline-delimited JSON. It is a stand-in
// for the OTLP exporter later phases wire up — the filtering guarantee, not
// the wire format, is what Phase 1 is proving.
type Exporter struct {
	w io.Writer
	// Drops counts every Emit's attempted-vs-dropped attribute keys (Phase
	// 10, README task 10.12's "telemetry attribute-drop rate") — nil is
	// valid and simply means this exporter isn't counted toward any
	// dashboard.
	Drops *DropTracker
}

func NewExporter(w io.Writer) *Exporter {
	return &Exporter{w: w}
}

// Emit filters attrs through the allowlist before writing — the exporter
// itself cannot bypass Filter, because it never sees anything else.
func (e *Exporter) Emit(name string, attrs Attrs) error {
	span := spanLine{Name: name, Attrs: FilterTracked(attrs, e.Drops)}
	enc := json.NewEncoder(e.w)
	return enc.Encode(span)
}

// StartSpan implements Tracer for the stdout fallback (docs/local-llm.md): a
// plain io.Writer has no real span/duration concept, so this just remembers
// the start time and defers writing one NDJSON line until End — the
// zero-setup `make run` demo path (no OTLP/Langfuse configured) still shows
// something per span, kind and elapsed time included, rather than silently
// doing nothing. ctx is returned unchanged: nesting is meaningful only to a
// real trace backend (otlp.go), not to this flat NDJSON stream.
func (e *Exporter) StartSpan(ctx context.Context, name string, kind ObservationType, attrs Attrs) (context.Context, Span) {
	return ctx, &stdoutSpan{e: e, name: name, kind: kind, attrs: attrs, start: time.Now()}
}

// Detach/Attach are no-ops: this exporter has no cross-goroutine
// propagation of its own (StartSpan's own doc comment — ctx is returned
// unchanged already), so there is nothing for either to do.
func (e *Exporter) Detach(context.Context) SpanLink                        { return nil }
func (e *Exporter) Attach(ctx context.Context, _ SpanLink) context.Context { return ctx }

type stdoutSpan struct {
	e             *Exporter
	name          string
	kind          ObservationType
	attrs         Attrs
	start         time.Time
	input, output string
}

func (s *stdoutSpan) End(attrs Attrs) {
	merged := make(Attrs, len(s.attrs)+len(attrs))
	for k, v := range s.attrs {
		merged[k] = v
	}
	for k, v := range attrs {
		merged[k] = v
	}
	span := spanLine{
		Name:       s.name,
		Attrs:      FilterTracked(merged, s.e.Drops),
		Kind:       s.kind,
		DurationMS: time.Since(s.start).Milliseconds(),
		Input:      s.input,
		Output:     s.output,
	}
	enc := json.NewEncoder(s.e.w)
	_ = enc.Encode(span)
}

// SetContent stashes input/output for End to write out — see Span.SetContent's
// own doc comment (tracer.go) for why this is a separate, explicitly-opt-in
// channel rather than something routed through FilterTracked.
func (s *stdoutSpan) SetContent(input, output string) {
	s.input, s.output = input, output
}
