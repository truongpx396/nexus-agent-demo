package obs

import (
	"context"
	"errors"
)

// target is the shape every concrete exporter in this package already
// satisfies (Exporter, OTLPExporter, LangfuseExporter) — MultiExporter
// composes N of these. Declared here rather than exported: the same "name
// the shape, not a concrete type" idiom rest.SpanEmitter
// (internal/surfaces/rest) already uses one layer up, for the identical
// reason (a caller passes concrete *OTLPExporter/*LangfuseExporter values,
// never needs to name this interface itself).
type target interface {
	Tracer
	Emit(name string, attrs Attrs) error
}

// MultiExporter fans a single logical span tree out to N underlying
// exporters at once (e.g. Grafana Tempo via OTLP AND a self-hosted
// Langfuse via native ingestion, simultaneously — docs/local-llm.md) — the
// composition none of this package's other exporters support on their
// own, each being built around exactly one destination.
//
// The one thing that makes this more than a bare loop over `targets`:
// OTLPExporter and LangfuseExporter both propagate parent/child linkage
// through the SAME go.opentelemetry.io/otel/trace.SpanContext slot in
// ctx. Calling both with the identical ctx would let whichever one runs
// second silently overwrite that shared slot, so a later child of the
// FIRST one would read the SECOND one's span as its parent — a span
// identity its own backend never received, corrupting nesting exactly for
// the case that matters most (delegated subagent runs, which cross a
// goroutine boundary via Tracer.Detach/Attach — internal/delegate/spawn.go).
// StartSpan/Detach/Attach below give each target its OWN private ctx
// thread instead, keyed by position in `targets`, so they never see each
// other's identity.
type MultiExporter struct {
	targets []target
}

// NewMultiExporter composes targets (at least one real exporter — this is
// meant to wrap OTLPExporter/LangfuseExporter/Exporter values built
// elsewhere, not to be handed zero of them) into one Tracer/Emitter.
func NewMultiExporter(targets ...target) *MultiExporter {
	return &MultiExporter{targets: targets}
}

// Emit fans out to every target — one failing must never stop the others,
// so every target always gets called regardless of an earlier one's
// error; the combined error (nil if every target succeeded) is
// errors.Join of whatever came back.
func (m *MultiExporter) Emit(name string, attrs Attrs) error {
	var errs []error
	for _, t := range m.targets {
		if err := t.Emit(name, attrs); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type multiCtxKey struct{}

// multiState carries one ctx per target — MultiExporter's own ctx
// representation, read back by the next StartSpan/Detach call so each
// target only ever sees ITS OWN prior return value as parent, never
// another target's (see the type's own doc comment for why that matters).
type multiState struct {
	perTarget []context.Context
}

// perTargetCtx returns the ctx target i should see as its parent: prior's
// own recorded ctx for i if MultiExporter has been through this ctx
// before, otherwise the bare incoming ctx (the root-span case — every
// target correctly sees "no parent" the same way).
func perTargetCtx(ctx context.Context, prior *multiState, i int) context.Context {
	if prior != nil && i < len(prior.perTarget) {
		return prior.perTarget[i]
	}
	return ctx
}

func (m *MultiExporter) StartSpan(ctx context.Context, name string, kind ObservationType, attrs Attrs) (context.Context, Span) {
	prior, _ := ctx.Value(multiCtxKey{}).(*multiState)
	next := &multiState{perTarget: make([]context.Context, len(m.targets))}
	spans := make([]Span, len(m.targets))
	for i, t := range m.targets {
		c, s := t.StartSpan(perTargetCtx(ctx, prior, i), name, kind, attrs)
		next.perTarget[i] = c
		spans[i] = s
	}
	return context.WithValue(ctx, multiCtxKey{}, next), &multiSpan{spans: spans}
}

func (m *MultiExporter) Detach(ctx context.Context) SpanLink {
	prior, _ := ctx.Value(multiCtxKey{}).(*multiState)
	links := make([]SpanLink, len(m.targets))
	anyValid := false
	for i, t := range m.targets {
		links[i] = t.Detach(perTargetCtx(ctx, prior, i))
		if links[i] != nil {
			anyValid = true
		}
	}
	if !anyValid {
		return nil
	}
	return links
}

func (m *MultiExporter) Attach(ctx context.Context, link SpanLink) context.Context {
	links, ok := link.([]SpanLink)
	if !ok {
		return ctx
	}
	next := &multiState{perTarget: make([]context.Context, len(m.targets))}
	for i, t := range m.targets {
		var l SpanLink
		if i < len(links) {
			l = links[i]
		}
		next.perTarget[i] = t.Attach(ctx, l)
	}
	return context.WithValue(ctx, multiCtxKey{}, next)
}

// multiSpan fans End/SetContent out to every target's own Span — the same
// per-target independence StartSpan already established, just closing it
// out.
type multiSpan struct {
	spans []Span
}

func (s *multiSpan) End(attrs Attrs) {
	for _, span := range s.spans {
		span.End(attrs)
	}
}

func (s *multiSpan) SetContent(input, output string) {
	for _, span := range s.spans {
		span.SetContent(input, output)
	}
}

// shutdowner is what OTLPExporter/LangfuseExporter both already implement
// (Exporter, the stdout one, does not — nothing to flush) — a plain
// type-assertion rather than adding Shutdown to the target interface
// itself, since not every target has one.
type shutdowner interface {
	Shutdown(ctx context.Context) error
}

// Shutdown flushes every target that has one to flush, tolerating any
// individual failure the same way Emit does.
func (m *MultiExporter) Shutdown(ctx context.Context) error {
	var errs []error
	for _, t := range m.targets {
		if s, ok := t.(shutdowner); ok {
			if err := s.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
