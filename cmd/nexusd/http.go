package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/truongpx396/nexus-agent-demo/internal/queue"
	queueredis "github.com/truongpx396/nexus-agent-demo/internal/queue/redis"
	"github.com/truongpx396/nexus-agent-demo/internal/reporting"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/cost"
)

// runHTTPServer serves handler on addr until ctx is cancelled, then drains it:
// README task 13.2 (F2), a real *http.Server with timeouts driven to a graceful
// Shutdown by ctx (runServe's signal.NotifyContext), not a bare ListenAndServe
// that never returns.
func runHTTPServer(ctx context.Context, addr string, handler http.Handler) error {
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout is deliberately 0 (unbounded): it bounds the ENTIRE
		// response write, including GET /v1/runs/{id}/events' long-lived
		// SSE stream — any nonzero value here would sever a legitimate,
		// still-active stream, not just a slow one. ReadHeaderTimeout/
		// ReadTimeout still bound slowloris on the request side, and
		// IdleTimeout still bounds a connection sitting idle between
		// requests.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}
	// ListenAndServe returns the instant Shutdown is called, not when the drain
	// finishes, so this must wait for the goroutine below or the process exits
	// (running the caller's defers and then main's return) with requests still in
	// flight. serveCtx also releases that goroutine when ListenAndServe fails on
	// its own (the port is taken), where ctx is never cancelled.
	serveCtx, stopServing := context.WithCancel(ctx)
	defer stopServing()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-serveCtx.Done()
		// serveCtx is already cancelled here, so the shutdown deadline hangs off a copy that keeps its values but not its cancellation.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(serveCtx), 30*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Error().Err(err).Msg("nexusd: graceful shutdown failed")
		}
	}()
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-drained
	return nil
}

// metricsStaleClaimAfter mirrors runDashboard's own default — how old an
// in_flight claim must be before /metrics counts it as unresolved (README
// task 6.6).
const metricsStaleClaimAfter = 15 * time.Minute

// handleMetrics exposes internal/obs.ComputeGoldenSignals' own per-tenant
// signals in Prometheus text exposition format (README task 13.12, closing
// production-readiness finding F13: "nothing scrapes, alerts, or pages" —
// this is what a scrape target actually needs). Hand-rolled rather than a
// client library: the format is a handful of "# TYPE"/metric lines, not
// worth a new dependency for. Like `nexusd dashboard`'s own CLI
// presentation, no DropTracker is threaded through here, so
// nexus_telemetry_attr_drop_rate always reports 0 from this endpoint
// specifically — the same "not measured here, never measured zero"
// documented limitation.
func handleMetrics(reports reporting.Reports, depth queue.Stater) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantIDs, err := tenantDirectory().ListTenantIDs(r.Context())
		if err != nil {
			log.Error().Err(err).Msg("nexusd: /metrics: list tenants")
			http.Error(w, "list tenants: internal error", http.StatusInternalServerError)
			return
		}

		signalsByTenant := make(map[uuid.UUID]reporting.GoldenSignals, len(tenantIDs))
		toolCallsByTenant := make(map[uuid.UUID]reporting.ToolCallCounts, len(tenantIDs))
		modelSpendByTenant := make(map[uuid.UUID][]reporting.ModelSpend, len(tenantIDs))
		for _, tenantID := range tenantIDs {
			signals, err := reporting.ComputeGoldenSignals(r.Context(), reports, tenantID, metricsStaleClaimAfter, nil, nil)
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
			toolCalls, err := reports.ToolCallCounts(r.Context(), tenantID)
			if err != nil {
				log.Error().Err(err).Any("tenant_id", tenantID).Msg("nexusd: /metrics: compute tool call counts")
			} else {
				toolCallsByTenant[tenantID] = toolCalls
			}

			modelSpend, err := reports.ModelSpend(r.Context(), tenantID)
			if err != nil {
				log.Error().Err(err).Any("tenant_id", tenantID).Msg("nexusd: /metrics: compute model spend")
			} else {
				modelSpendByTenant[tenantID] = modelSpend
			}
		}

		w.Header().Set("content-type", "text/plain; version=0.0.4")

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

		writeQueueGauges(r.Context(), w, depth)

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
	}
}

func writeGaugeHeader(w http.ResponseWriter, name, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help) //nolint:errcheck // best-effort write to a scrape response
	fmt.Fprintf(w, "# TYPE %s gauge\n", name)    //nolint:errcheck // best-effort write to a scrape response
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz reports ready only once every dependency this process
// actually needs to serve a request is reachable: Postgres (through
// PgBouncer, the same pool serve() itself uses), Redis (the cost gate's
// counter store) AND its own queue consumer group (queueredis.CheckReady —
// "Redis answers a PING" isn't the same as "the job queue this process
// enqueues into/leases from actually exists"), and signerd (the audit
// chain's only path to a signature — runGoLive's item 2b already performs
// this exact check, once, at CLI time; this is the same check as a live
// HTTP probe).
func handleReadyz(pool *pgxpool.Pool, redisClient *redis.Client, signer *audit.SignerClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if err := pool.Ping(ctx); err != nil {
			log.Warn().Err(err).Msg("nexusd: /readyz: postgres not reachable")
			http.Error(w, "postgres not reachable", http.StatusServiceUnavailable)
			return
		}
		if err := redisClient.Ping(ctx).Err(); err != nil {
			log.Warn().Err(err).Msg("nexusd: /readyz: redis not reachable")
			http.Error(w, "redis not reachable", http.StatusServiceUnavailable)
			return
		}
		if err := queueredis.CheckReady(ctx, redisClient); err != nil {
			log.Warn().Err(err).Msg("nexusd: /readyz: queue not ready")
			http.Error(w, "queue not ready", http.StatusServiceUnavailable)
			return
		}
		if _, _, err := signer.PublicKey(ctx); err != nil {
			log.Warn().Err(err).Msg("nexusd: /readyz: signerd not reachable")
			http.Error(w, "signerd not reachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
