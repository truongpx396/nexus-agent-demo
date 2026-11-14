package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	queueredis "github.com/truongpx396/nexus-agent/internal/queue/redis"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent/internal/audit"
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
