package obs

import (
	"context"
	"fmt"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// OTLPExporter sends filtered spans to a real OTLP collector over HTTP
// (README task 13.12, closing production-readiness finding F13: "no
// telemetry leaves the process"). Same Emit(name, Attrs) shape as the
// stdout Exporter (span.go) — a caller swaps one for the other with no
// other code change — and the filtering guarantee is identical: both route
// every attribute through FilterTracked/the same deny-by-default allowlist
// before it can leave the process. This is the adapter the filtering seam
// was always meant to have; the allowlist itself is unchanged.
//
// It also implements Tracer (tracer.go), the span-nesting counterpart to
// Emit that kernel/loop.go uses (docs/local-llm.md) to give Langfuse — or
// any other OTLP-native backend, this is not Langfuse-specific — real
// per-step visibility into a run.
type OTLPExporter struct {
	tracer trace.Tracer
	tp     *sdktrace.TracerProvider
	// Drops mirrors the stdout Exporter's own field — nil is valid and
	// simply means this exporter isn't counted toward any dashboard.
	Drops *DropTracker
}

// NewOTLPExporter dials endpoint (host:port, no scheme — e.g.
// "otel-collector:4318") over OTLP/HTTP. insecure disables TLS, the honest
// default for a collector reached over a private network the same way
// PgBouncer/Redis already are in this deployment; a real TLS collector
// endpoint is a config change to this one call, not a code change.
//
// headers and urlPath are both optional (nil / "" for a collector that
// needs neither, e.g. the default local stdout-alternative setup) — added
// specifically so this same constructor can reach Langfuse's OTLP endpoint
// (docs/local-llm.md), which needs a Basic-auth header and a non-default
// URL path (`/api/public/otel/v1/traces`, not the OTel default `/v1/traces`).
func NewOTLPExporter(ctx context.Context, endpoint string, insecure bool, headers map[string]string, urlPath string) (*OTLPExporter, error) {
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}
	if insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	if len(headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(headers))
	}
	if urlPath != "" {
		opts = append(opts, otlptracehttp.WithURLPath(urlPath))
	}
	client := otlptracehttp.NewClient(opts...)
	exp, err := otlptrace.New(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("obs: connect OTLP exporter at %s: %w", endpoint, err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	return &OTLPExporter{tracer: tp.Tracer("nexusd"), tp: tp}, nil
}

// Emit filters attrs through the allowlist before it ever becomes a span
// attribute — the exporter itself cannot bypass Filter, because it never
// sees anything else (span.go's own Exporter.Emit doc comment, verbatim).
func (e *OTLPExporter) Emit(name string, attrs Attrs) error {
	filtered := FilterTracked(attrs, e.Drops)
	_, span := e.tracer.Start(context.Background(), name)
	span.SetAttributes(toAttributeKVs(filtered)...)
	span.End()
	return nil
}

// StartSpan implements Tracer: opens a real OTel span using the CALLER's
// ctx (not context.Background(), unlike Emit above) — standard OTel
// parent/child propagation means a child span started with the ctx this
// returns nests correctly under it automatically, no manual trace-ID
// plumbing required.
//
// Beyond the allowlist-filtered attrs every span already carries, this sets
// two things Langfuse's OTel ingestion specifically reads to render its
// trace/agent-graph view (confirmed against Langfuse's own docs,
// docs/local-llm.md): the `langfuse.observation.type` attribute (from kind
// — Langfuse's own highest-priority signal for how to draw a span), and,
// for a generation span only, OTel GenAI semantic-convention ALIASES of the
// already-allowlisted model.id/usage.* keys (gen_ai.request.model,
// gen_ai.usage.{input,output}_tokens) — added alongside the originals,
// never instead of them. gen_ai.prompt/gen_ai.completion are never set:
// content stays out of this path exactly like Emit's (constitution
// Principle VI) — this only relabels structure that was already
// allowlisted, it does not admit anything new.
func (e *OTLPExporter) StartSpan(ctx context.Context, name string, kind ObservationType, attrs Attrs) (context.Context, Span) {
	filtered := FilterTracked(attrs, e.Drops)
	kvs := toAttributeKVs(filtered)
	kvs = append(kvs, attribute.String("langfuse.observation.type", string(kind)))
	kvs = append(kvs, langfuseGenAIAliases(kind, filtered)...)
	spanCtx, span := e.tracer.Start(ctx, name, trace.WithAttributes(kvs...))
	return spanCtx, &otlpSpan{span: span, kind: kind, drops: e.Drops}
}

type otlpSpan struct {
	span  trace.Span
	kind  ObservationType
	drops *DropTracker
}

// End merges attrs known only at close (usage, outcome, terminal_reason)
// onto the span before ending it — filtered and GenAI-aliased exactly like
// StartSpan's own attrs.
func (s *otlpSpan) End(attrs Attrs) {
	if len(attrs) > 0 {
		filtered := FilterTracked(attrs, s.drops)
		kvs := toAttributeKVs(filtered)
		kvs = append(kvs, langfuseGenAIAliases(s.kind, filtered)...)
		s.span.SetAttributes(kvs...)
	}
	s.span.End()
}

// SetContent sets Langfuse's universal input/output attributes directly —
// bypassing FilterTracked/the allowlist entirely, on purpose (Span.SetContent's
// own doc comment: this is a separate, explicitly-opt-in channel, not a way
// around Filter). Empty strings are skipped rather than sent as
// zero-length attributes, so a caller that has content for only one side
// (e.g. a call that errored before producing output) doesn't manufacture a
// misleading empty value for the other.
func (s *otlpSpan) SetContent(input, output string) {
	var kvs []attribute.KeyValue
	if input != "" {
		kvs = append(kvs, attribute.String("langfuse.observation.input", input))
	}
	if output != "" {
		kvs = append(kvs, attribute.String("langfuse.observation.output", output))
	}
	if len(kvs) > 0 {
		s.span.SetAttributes(kvs...)
	}
}

func toAttributeKVs(attrs Attrs) []attribute.KeyValue {
	kvs := make([]attribute.KeyValue, 0, len(attrs))
	for k, v := range attrs {
		kvs = append(kvs, attribute.String(k, v))
	}
	return kvs
}

// langfuseGenAIAliases adds OTel GenAI semantic-convention aliases for a
// generation span's already-allowlisted model.id/usage.* keys — see
// StartSpan's own doc comment for why (Langfuse specifically reads these,
// confirmed via its docs). Every other observation kind gets none: a tool
// or agent span carries no token usage to alias.
func langfuseGenAIAliases(kind ObservationType, attrs Attrs) []attribute.KeyValue {
	if kind != ObservationGeneration {
		return nil
	}
	var kvs []attribute.KeyValue
	if v, ok := attrs["model.id"]; ok {
		kvs = append(kvs, attribute.String("gen_ai.request.model", v))
	}
	if input := sumInt(attrs, "usage.input_uncached", "usage.input_cache_read", "usage.input_cache_write"); input > 0 {
		kvs = append(kvs, attribute.Int64("gen_ai.usage.input_tokens", input))
	}
	if v, ok := attrs["usage.output"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			kvs = append(kvs, attribute.Int64("gen_ai.usage.output_tokens", n))
		}
	}
	return kvs
}

func sumInt(attrs Attrs, keys ...string) int64 {
	var total int64
	for _, k := range keys {
		if v, ok := attrs[k]; ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				total += n
			}
		}
	}
	return total
}

// Shutdown flushes every buffered span before the process exits — a
// caller's responsibility, the same way *sql.DB.Close or an http.Server's
// own Shutdown are: nothing in this package calls it automatically.
func (e *OTLPExporter) Shutdown(ctx context.Context) error {
	return e.tp.Shutdown(ctx)
}
