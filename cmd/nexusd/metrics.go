package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/queue"
	"github.com/truongpx396/nexus-agent-demo/internal/reporting"
)

// metricsStaleClaimAfter mirrors runDashboard's own default — how old an
// in_flight claim must be before /metrics counts it as unresolved (README
// task 6.6).
const metricsStaleClaimAfter = 15 * time.Minute

// renderMetrics writes internal/obs.ComputeGoldenSignals' own per-tenant
// signals in Prometheus text exposition format (README task 13.12, closing
// production-readiness finding F13: "nothing scrapes, alerts, or pages" —
// this is what a scrape target actually needs). Hand-rolled rather than a
// client library: the format is a handful of "# TYPE"/metric lines, not
// worth a new dependency for. Like `nexusd dashboard`'s own CLI
// presentation, no DropTracker is threaded through here, so
// nexus_telemetry_attr_drop_rate always reports 0 from this endpoint
// specifically — the same "not measured here, never measured zero"
// documented limitation.
//
// It writes to a buffer the handler caches (metricsHandler), not to a
// response, so one render serves every scrape in the cache window.
func renderMetrics(ctx context.Context, w io.Writer, tenants tenantLister, reports reporting.Reports, depth queue.Stater) error {
	tenantIDs, err := tenants(ctx)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}

	signalsByTenant := make(map[uuid.UUID]reporting.GoldenSignals, len(tenantIDs))
	toolCallsByTenant := make(map[uuid.UUID]reporting.ToolCallCounts, len(tenantIDs))
	modelSpendByTenant := make(map[uuid.UUID][]reporting.ModelSpend, len(tenantIDs))
	for _, tenantID := range tenantIDs {
		signals, err := reporting.ComputeGoldenSignals(ctx, reports, tenantID, metricsStaleClaimAfter, nil, nil)
		if err != nil {
			log.Error().Err(err).Any("tenant_id", tenantID).Msg("nexusd: /metrics: compute golden signals")
			continue
		}
		signalsByTenant[tenantID] = signals

		// Per-tool / per-model breakdown (specs/README.md Phase 17,
		// task 17.9) — additive to the golden-signal aggregates above, not
		// a replacement: a failure here is logged and skipped, never lets
		// a breakdown query take down the golden-signal scrape it rides
		// alongside.
		toolCalls, err := reports.ToolCallCounts(ctx, tenantID)
		if err != nil {
			log.Error().Err(err).Any("tenant_id", tenantID).Msg("nexusd: /metrics: compute tool call counts")
		} else {
			toolCallsByTenant[tenantID] = toolCalls
		}

		modelSpend, err := reports.ModelSpend(ctx, tenantID)
		if err != nil {
			log.Error().Err(err).Any("tenant_id", tenantID).Msg("nexusd: /metrics: compute model spend")
		} else {
			modelSpendByTenant[tenantID] = modelSpend
		}
	}

	writeGaugeHeader(w, "nexus_completion_rate", "Fraction of terminal sessions ending under a given terminal_reason.")
	for tenantID, signals := range signalsByTenant {
		for reason, rate := range signals.CompletionRateByReason {
			fmt.Fprintf(w, "nexus_completion_rate{tenant_id=%q,reason=%q} %f\n", tenantID, reason, rate) //nolint:errcheck // best-effort write to a scrape response
		}
	}

	for _, m := range []struct {
		name, help string
		value      func(reporting.GoldenSignals) float64
	}{
		{"nexus_stuck_rate", "Fraction of terminal sessions ending stuck_terminated.", func(s reporting.GoldenSignals) float64 { return s.StuckRate }},
		{"nexus_cost_ceiling_breach_rate", "Fraction of budget_decisions resolved refuse_ceiling.", func(s reporting.GoldenSignals) float64 { return s.CostCeilingBreachRate }},
		{"nexus_cache_read_rate", "Fraction of input tokens served from cache.", func(s reporting.GoldenSignals) float64 { return s.CacheReadRate }},
		{"nexus_approval_p50_decision_ms", "Median approval decision latency in milliseconds.", func(s reporting.GoldenSignals) float64 { return float64(s.ApprovalP50DecisionMS) }},
		{"nexus_approval_p95_decision_ms", "P95 approval decision latency in milliseconds.", func(s reporting.GoldenSignals) float64 { return float64(s.ApprovalP95DecisionMS) }},
		{"nexus_approval_mismatch_rate", "Fraction of decided approvals that resolved approval_mismatch.", func(s reporting.GoldenSignals) float64 { return s.ApprovalMismatchRate }},
		{"nexus_unresolved_inflight_claims", "In-flight claims older than the staleness window.", func(s reporting.GoldenSignals) float64 { return float64(s.UnresolvedInFlightClaims) }},
		{"nexus_telemetry_attr_drop_rate", "Fraction of telemetry attribute keys dropped by the allowlist (not measured by this endpoint; see internal/obs.DropTracker).", func(s reporting.GoldenSignals) float64 { return s.TelemetryAttrDropRate }},
		{"nexus_taint_transition_rate", "Fraction of terminal sessions with at least one taint_transition event (constitution Principle V's Rule of Two).", func(s reporting.GoldenSignals) float64 { return s.TaintTransitionRate }},
	} {
		writeGaugeHeader(w, m.name, m.help)
		for tenantID, signals := range signalsByTenant {
			fmt.Fprintf(w, "%s{tenant_id=%q} %f\n", m.name, tenantID, m.value(signals)) //nolint:errcheck // best-effort write to a scrape response
		}
	}

	writeGaugeHeader(w, "nexus_tool_call_count", "tool_use events recorded per tool (events.tool_id — structural, never the encrypted payload).")
	for tenantID, toolCalls := range toolCallsByTenant {
		for toolID, n := range toolCalls {
			fmt.Fprintf(w, "nexus_tool_call_count{tenant_id=%q,tool_id=%q} %f\n", tenantID, toolID, float64(n)) //nolint:errcheck // best-effort write to a scrape response
		}
	}

	writeQueueGauges(ctx, w, depth)

	writeGaugeHeader(w, "nexus_sessions_by_surface", "Session count grouped by originating surface_id (web/telegram/zalo/email/cli/api).")
	for tenantID, signals := range signalsByTenant {
		for surfaceID, n := range signals.SessionsBySurface {
			fmt.Fprintf(w, "nexus_sessions_by_surface{tenant_id=%q,surface_id=%q} %f\n", tenantID, surfaceID, float64(n)) //nolint:errcheck // best-effort write to a scrape response
		}
	}

	writeGaugeHeader(w, "nexus_model_input_tokens", "Reconciled input tokens per routed model, split by cache class (uncached/cache_read/cache_write).")
	writeGaugeHeader(w, "nexus_model_output_tokens", "Reconciled output tokens per routed model.")
	// currencyMajorUnits converts cost.Money's exact integer minor units
	// (cost.Micros: 1 currency unit = 1_000_000 micros) to a float64 ONLY
	// here, at the Prometheus text-exposition boundary — the one place
	// this endpoint's own doc comment already treats as sanctioned for
	// every other rate above, never inside internal/cost or internal/obs
	// itself (task 4.1's float64 ban).
	writeGaugeHeader(w, "nexus_model_cost", "Reconciled cost per routed model, in major currency units (see the currency label).")
	for tenantID, spend := range modelSpendByTenant {
		for _, m := range spend {
			fmt.Fprintf(w, "nexus_model_input_tokens{tenant_id=%q,model_id=%q,class=\"uncached\"} %f\n", tenantID, m.ModelID, float64(m.InputUncached))                    //nolint:errcheck // best-effort write to a scrape response
			fmt.Fprintf(w, "nexus_model_input_tokens{tenant_id=%q,model_id=%q,class=\"cache_read\"} %f\n", tenantID, m.ModelID, float64(m.InputCacheRead))                 //nolint:errcheck // best-effort write to a scrape response
			fmt.Fprintf(w, "nexus_model_input_tokens{tenant_id=%q,model_id=%q,class=\"cache_write\"} %f\n", tenantID, m.ModelID, float64(m.InputCacheWrite))               //nolint:errcheck // best-effort write to a scrape response
			fmt.Fprintf(w, "nexus_model_output_tokens{tenant_id=%q,model_id=%q} %f\n", tenantID, m.ModelID, float64(m.Output))                                             //nolint:errcheck // best-effort write to a scrape response
			fmt.Fprintf(w, "nexus_model_cost{tenant_id=%q,model_id=%q,currency=%q} %f\n", tenantID, m.ModelID, m.Currency, float64(m.CostMinorUnits)/float64(cost.Micros)) //nolint:errcheck // best-effort write to a scrape response
		}
	}
	return nil
}

func writeGaugeHeader(w io.Writer, name, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help) //nolint:errcheck // best-effort write to a scrape response
	fmt.Fprintf(w, "# TYPE %s gauge\n", name)    //nolint:errcheck // best-effort write to a scrape response
}

// tenantLister is how /metrics learns which tenants exist: the platform
// operator's cross-tenant directory in production, a stub in a test.
type tenantLister func(ctx context.Context) ([]uuid.UUID, error)

const (
	// metricsCacheTTL is how long a rendered scrape is served again. A render lists
	// every tenant and runs several queries for each, so scrapes inside the window
	// share one instead of each loading the database.
	metricsCacheTTL = 15 * time.Second
	// metricsRenderTimeout bounds one render, which runs detached from the scrape
	// that triggered it (see render).
	metricsRenderTimeout = 30 * time.Second
)

// metricsHandler serves /metrics from a cached render. One render at a time:
// scrapes that arrive while it runs wait for it and take its result.
type metricsHandler struct {
	tenants tenantLister
	reports reporting.Reports
	depth   queue.Stater
	ttl     time.Duration    // 0 means metricsCacheTTL
	now     func() time.Time // nil means time.Now; a test seam

	mu   sync.Mutex
	at   time.Time
	body []byte
}

func newMetricsHandler(tenants tenantLister, reports reporting.Reports, depth queue.Stater) *metricsHandler {
	return &metricsHandler{tenants: tenants, reports: reports, depth: depth}
}

func (m *metricsHandler) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *metricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := m.render(r.Context())
	if err != nil {
		// The cause is in the log; a scrape response never carries an infrastructure error.
		log.Error().Err(err).Msg("nexusd: /metrics: render")
		http.Error(w, "render metrics: internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/plain; version=0.0.4")
	_, _ = w.Write(body) // best-effort write to a scrape response
}

// render returns the cached body while it is younger than the TTL, and renders a
// fresh one otherwise. A failed render is returned and not cached.
func (m *metricsHandler) render(ctx context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ttl := m.ttl
	if ttl <= 0 {
		ttl = metricsCacheTTL
	}
	if m.body != nil && m.clock().Sub(m.at) < ttl {
		return m.body, nil
	}

	// The render outlives the scrape that started it: if that client hangs up
	// part-way, every query after that point would fail and a body with the tenants
	// missing would be served to everyone else for the whole window.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), metricsRenderTimeout)
	defer cancel()
	var buf bytes.Buffer
	if err := renderMetrics(ctx, &buf, m.tenants, m.reports, m.depth); err != nil {
		return nil, err
	}
	m.body, m.at = buf.Bytes(), m.clock()
	return m.body, nil
}

// requireBearer wraps next so it answers only a request carrying "Authorization:
// Bearer <token>", compared in constant time, and answers 401 otherwise. An empty
// token passes everything through, which only --dev ever configures: outside it,
// loadServerConfig refuses to start without NEXUS_METRICS_TOKEN.
func requireBearer(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, got, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexusd metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
