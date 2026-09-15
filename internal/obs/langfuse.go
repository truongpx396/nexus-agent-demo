package obs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/trace"
)

// LangfuseExporter sends filtered spans to a self-hosted Langfuse over its
// legacy native Ingestion API (docs/local-llm.md) instead of OTLP
// (otlp.go) — the lightweight alternative for a self-hosted Langfuse v2
// (web + postgres only, no ClickHouse/MinIO/worker), which predates OTLP
// ingestion entirely (confirmed: POST /api/public/otel/v1/traces → 404
// against v2). This is a deliberately different protocol from otlp.go, not
// a config variant of it — Langfuse's own docs mark this Ingestion API
// deprecated in favor of OTLP (Cloud sunset 2026-11-16), a trade-off
// accepted here specifically to keep the local trace stack at 2 containers
// instead of 6.
//
// Same Tracer/Span contract, same FilterTracked allowlist discipline as
// every other exporter in this package — the filtering guarantee is
// identical, only the wire format and the exporter-chosen destination
// differ.
type LangfuseExporter struct {
	host                 string
	publicKey, secretKey string
	client               *http.Client

	// Drops mirrors the other exporters' own field — nil is valid and simply
	// means this exporter isn't counted toward any dashboard.
	Drops *DropTracker

	mu      sync.Mutex
	pending []langfuseEvent

	flushInterval time.Duration
	stop          chan struct{}
	stopped       chan struct{}
}

// langfuseEvent is the Ingestion API's batch envelope shape, verbatim
// (Langfuse's fern/apis/server/definition/ingestion.yml): every event in a
// batch carries its own id/timestamp plus a `type` discriminator and a
// type-specific body.
type langfuseEvent struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Body      any    `json:"body"`
}

// langfuseTraceBody is TraceBody (trace-create) — one per run, opened by
// the root (ObservationAgent) span.
type langfuseTraceBody struct {
	ID       string            `json:"id,omitempty"`
	Name     string            `json:"name,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// langfuseObservationBody covers span/generation/event create+update alike
// (CreateSpanBody/UpdateSpanBody/CreateGenerationBody/UpdateGenerationBody/
// CreateEventBody in Langfuse's schema all share this same field set;
// omitempty naturally narrows it to whichever subset a given event type
// actually uses). Model/Usage are populated only for a generation.
//
// Usage is the ORIGINAL/legacy usage field, not the newer usageDetails/
// costDetails documented on Langfuse's current (v3+) schema — deliberately,
// since this targets an old v2 server image: confirmed against a real
// langfuse/langfuse:2 instance (docs/local-llm.md) that usage alone is
// enough — it correctly populates Langfuse's own promptTokens/
// completionTokens/totalTokens and cost UI, no need for usageDetails too.
type langfuseObservationBody struct {
	ID                  string            `json:"id,omitempty"`
	TraceID             string            `json:"traceId,omitempty"`
	ParentObservationID string            `json:"parentObservationId,omitempty"`
	Name                string            `json:"name,omitempty"`
	StartTime           string            `json:"startTime,omitempty"`
	EndTime             string            `json:"endTime,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
	Input               string            `json:"input,omitempty"`
	Output              string            `json:"output,omitempty"`
	Model               string            `json:"model,omitempty"`
	Usage               *langfuseUsage    `json:"usage,omitempty"`
}

type langfuseUsage struct {
	Input  int64  `json:"input,omitempty"`
	Output int64  `json:"output,omitempty"`
	Unit   string `json:"unit,omitempty"`
}

type langfuseBatchRequest struct {
	Batch []langfuseEvent `json:"batch"`
}

// NewLangfuseExporter builds a client against a self-hosted Langfuse at
// host (e.g. "http://localhost:3001"), authenticating every ingestion call
// with Basic auth (publicKey, secretKey) — Langfuse's own convention for
// this API. Unlike NewOTLPExporter there is no connection handshake here
// (no SDK to dial); construction just starts the background flusher.
func NewLangfuseExporter(host, publicKey, secretKey string) *LangfuseExporter {
	return newLangfuseExporterWithInterval(host, publicKey, secretKey, time.Second)
}

// newLangfuseExporterWithInterval is NewLangfuseExporter with the flush
// interval as a constructor argument rather than a field a caller sets
// after the fact — flushLoop (started here, before this function returns)
// reads flushInterval on its very first iteration, so setting it via a
// plain `exp.flushInterval = ...` after NewLangfuseExporter returns races
// that read with no synchronization (caught by `go test -race`, which is
// exactly how the tests using this found it). Package-private since only
// this package's own tests need a non-default interval.
func newLangfuseExporterWithInterval(host, publicKey, secretKey string, flushInterval time.Duration) *LangfuseExporter {
	e := &LangfuseExporter{
		host:          host,
		publicKey:     publicKey,
		secretKey:     secretKey,
		client:        &http.Client{Timeout: 10 * time.Second},
		flushInterval: flushInterval,
		stop:          make(chan struct{}),
		stopped:       make(chan struct{}),
	}
	go e.flushLoop()
	return e
}

// flushLoop is the background, non-blocking counterpart to OTLPExporter's
// OTel-SDK-internal batching: StartSpan/End/Emit only ever enqueue (see
// enqueue below), so the kernel's hot path never blocks on this exporter's
// network I/O. Best-effort only — a failed flush is logged and dropped, no
// retry/backoff, consistent with this being an opt-in local-dev
// integration rather than a delivery guarantee.
func (e *LangfuseExporter) flushLoop() {
	defer close(e.stopped)
	ticker := time.NewTicker(e.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			e.flush(context.Background())
		case <-e.stop:
			e.flush(context.Background())
			return
		}
	}
}

func (e *LangfuseExporter) enqueue(ev langfuseEvent) {
	e.mu.Lock()
	e.pending = append(e.pending, ev)
	e.mu.Unlock()
}

func (e *LangfuseExporter) flush(ctx context.Context) {
	e.mu.Lock()
	batch := e.pending
	e.pending = nil
	e.mu.Unlock()
	if len(batch) == 0 {
		return
	}

	body, err := json.Marshal(langfuseBatchRequest{Batch: batch})
	if err != nil {
		log.Error().Err(err).Msg("obs: marshal langfuse ingestion batch")
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.host+"/api/public/ingestion", bytes.NewReader(body))
	if err != nil {
		log.Error().Err(err).Msg("obs: build langfuse ingestion request")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(e.publicKey, e.secretKey)

	resp, err := e.client.Do(req)
	if err != nil {
		log.Error().Err(err).Msg("obs: send langfuse ingestion batch")
		return
	}
	defer func() { _ = resp.Body.Close() }() // response body carries nothing this call site reads
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Error().Int("status", resp.StatusCode).Msg("obs: langfuse ingestion batch rejected")
	}
}

// Shutdown stops the background flusher and flushes whatever is still
// buffered, synchronously — the same contract OTLPExporter.Shutdown
// already has, which cmd/nexusd/main.go's newSpanExporter calls on process
// exit regardless of which exporter it built.
func (e *LangfuseExporter) Shutdown(ctx context.Context) error {
	close(e.stop)
	select {
	case <-e.stopped:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// Emit implements rest.SpanEmitter: one rootless event-create, mirroring
// OTLPExporter.Emit's own context.Background()-rooted behavior (this
// method takes no ctx, so it can never nest under a real run's trace).
// Sent with no traceId, same as a caller that has none to give it — Langfuse
// auto-creates an implicit single-observation trace for it (its own id
// doubling as that trace's id), confirmed against a real langfuse/
// langfuse:2 instance (docs/local-llm.md), not dropped.
func (e *LangfuseExporter) Emit(name string, attrs Attrs) error {
	filtered := FilterTracked(attrs, e.Drops)
	e.enqueue(langfuseEvent{
		ID:        uuid.NewString(),
		Timestamp: nowRFC3339(),
		Type:      "event-create",
		Body: langfuseObservationBody{
			ID:       uuid.NewString(),
			Name:     name,
			Metadata: filtered,
		},
	})
	return nil
}

type langfuseSpan struct {
	exp                   *LangfuseExporter
	traceID, id, parentID string
	kind                  ObservationType
	name                  string
	startTime             time.Time
	startAttrs            Attrs
	input, output         string
}

// StartSpan implements Tracer without any real OTLP export — but it still
// propagates parent/child linkage through ctx via go.opentelemetry.io/otel/
// trace's own SpanContext (trace.SpanContextFromContext/
// trace.ContextWithSpanContext), the SAME vendor-neutral primitive
// OTLPExporter's real spans carry, rather than a private ctx-key of its
// own. This is not cosmetic: internal/delegate/spawn.go carries a delegated
// subagent's parent span across a goroutine boundary using exactly this
// trace.SpanContext mechanism (trace.ContextWithRemoteSpanContext) so the
// child's own root span nests under the delegate call that spawned it
// instead of opening as a disconnected new trace — that cross-goroutine
// propagation only works if THIS exporter also speaks trace.SpanContext,
// which an earlier, private-ctx-key version of this method did not
// (confirmed empirically: parentSpanCtx.IsValid() was false across that
// boundary, and the child became a second, unlinked trace-create). Every
// Langfuse id sent below (traceId/id/parentObservationId) is just this
// SpanContext's own TraceID/SpanID rendered as hex via .String() — an
// arbitrary string is all Langfuse's ingestion API requires, so reusing
// OTel's own ID space costs nothing and buys real interop with
// spawn.go's existing propagation.
//
// No valid parent in ctx means this call opens a new trace (one
// trace-create, id = a fresh TraceID) as well as its own root observation;
// a valid parent means this reuses the parent's TraceID and links via
// parentObservationId = the parent's SpanID.
func (e *LangfuseExporter) StartSpan(ctx context.Context, name string, kind ObservationType, attrs Attrs) (context.Context, Span) {
	parentSC := trace.SpanContextFromContext(ctx)

	var traceID trace.TraceID
	parentID := ""
	if parentSC.IsValid() {
		traceID = parentSC.TraceID()
		parentID = parentSC.SpanID().String()
	} else {
		traceID = newTraceID()
	}
	spanID := newSpanID()

	filtered := FilterTracked(attrs, e.Drops)
	now := time.Now()

	if !parentSC.IsValid() {
		e.enqueue(langfuseEvent{
			ID:        uuid.NewString(),
			Timestamp: nowRFC3339(),
			Type:      "trace-create",
			Body:      langfuseTraceBody{ID: traceID.String(), Name: name, Metadata: filtered},
		})
	}

	body := langfuseObservationBody{
		ID:                  spanID.String(),
		TraceID:             traceID.String(),
		ParentObservationID: parentID,
		Name:                name,
		StartTime:           now.UTC().Format(time.RFC3339Nano),
		Metadata:            filtered,
	}
	eventType := "span-create"
	if kind == ObservationGeneration {
		eventType = "generation-create"
		body.Model, body.Usage = langfuseGenerationFields(filtered)
	}
	e.enqueue(langfuseEvent{ID: uuid.NewString(), Timestamp: nowRFC3339(), Type: eventType, Body: body})

	span := &langfuseSpan{
		exp:        e,
		traceID:    traceID.String(),
		id:         spanID.String(),
		parentID:   parentID,
		kind:       kind,
		name:       name,
		startTime:  now,
		startAttrs: attrs,
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(ctx, sc), span
}

// Detach/Attach: identical mechanism to OTLPExporter's own (otlp.go) —
// this exporter already keys its own parent/child linkage off the same
// trace.SpanContext, specifically so it interops with
// internal/delegate/spawn.go's cross-goroutine propagation without
// spawn.go needing to know which Tracer implementation is actually wired.
func (e *LangfuseExporter) Detach(ctx context.Context) SpanLink {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return sc
}

func (e *LangfuseExporter) Attach(ctx context.Context, link SpanLink) context.Context {
	sc, ok := link.(trace.SpanContext)
	if !ok {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}

func newTraceID() trace.TraceID {
	var id trace.TraceID
	_, _ = rand.Read(id[:])
	return id
}

func newSpanID() trace.SpanID {
	var id trace.SpanID
	_, _ = rand.Read(id[:])
	return id
}

// End sends the matching -update event: span-update for an agent/tool span,
// generation-update for a generation. Input/output land on this event too
// (only if SetContent was called — see its own doc comment) rather than on
// the create event, since a caller typically has content only once the
// call finishes.
func (s *langfuseSpan) End(attrs Attrs) {
	merged := make(Attrs, len(s.startAttrs)+len(attrs))
	for k, v := range s.startAttrs {
		merged[k] = v
	}
	for k, v := range attrs {
		merged[k] = v
	}
	filtered := FilterTracked(merged, s.exp.Drops)

	body := langfuseObservationBody{
		ID:       s.id,
		EndTime:  time.Now().UTC().Format(time.RFC3339Nano),
		Metadata: filtered,
		Input:    s.input,
		Output:   s.output,
	}
	eventType := "span-update"
	if s.kind == ObservationGeneration {
		eventType = "generation-update"
		body.Model, body.Usage = langfuseGenerationFields(filtered)
	}
	s.exp.enqueue(langfuseEvent{ID: uuid.NewString(), Timestamp: nowRFC3339(), Type: eventType, Body: body})
}

// SetContent stashes input/output for End to send — deliberately bypassing
// FilterTracked, exactly like otlpSpan.SetContent (tracer.go's own doc
// comment on Span.SetContent): this is a separate, explicitly opt-in
// content channel (NEXUS_TRACE_CONTENT), not a way around the allowlist.
func (s *langfuseSpan) SetContent(input, output string) {
	s.input, s.output = input, output
}

// langfuseGenerationFields translates a generation span's already-
// allowlisted model.id/usage.* keys into this event's model/usage fields —
// the native-ingestion counterpart to otlp.go's langfuseGenAIAliases. A
// non-generation span never calls this.
func langfuseGenerationFields(attrs Attrs) (string, *langfuseUsage) {
	model := attrs["model.id"]
	usage := &langfuseUsage{Unit: "TOKENS"}
	var has bool
	if v := sumInt(attrs, "usage.input_uncached", "usage.input_cache_read", "usage.input_cache_write"); v > 0 {
		usage.Input = v
		has = true
	}
	if v, ok := attrs["usage.output"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			usage.Output = n
			has = true
		}
	}
	if !has {
		usage = nil
	}
	return model, usage
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
