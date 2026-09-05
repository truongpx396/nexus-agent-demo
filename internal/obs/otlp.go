package obs

import (
	"context"
	"fmt"

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
func NewOTLPExporter(ctx context.Context, endpoint string, insecure bool) (*OTLPExporter, error) {
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}
	if insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
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
	kvs := make([]attribute.KeyValue, 0, len(filtered))
	for k, v := range filtered {
		kvs = append(kvs, attribute.String(k, v))
	}
	span.SetAttributes(kvs...)
	span.End()
	return nil
}

// Shutdown flushes every buffered span before the process exits — a
// caller's responsibility, the same way *sql.DB.Close or an http.Server's
// own Shutdown are: nothing in this package calls it automatically.
func (e *OTLPExporter) Shutdown(ctx context.Context) error {
	return e.tp.Shutdown(ctx)
}
