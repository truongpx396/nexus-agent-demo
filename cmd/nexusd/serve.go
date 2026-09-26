package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/authn"
	"github.com/truongpx396/nexus-agent-demo/internal/connectors"
	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	"github.com/truongpx396/nexus-agent-demo/internal/memory"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/reliability"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/cron"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/email"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/telegram"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/zalo"
	"github.com/truongpx396/nexus-agent-demo/internal/teams"
	"github.com/truongpx396/nexus-agent-demo/internal/version"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

func runServe() {
	fmt.Printf("nexusd %s (%s)\n", version.Version, version.GitCommit)

	// signal.NotifyContext (README task 13.2, F2) — mirrors
	// cmd/signerd/main.go's own pattern: SIGTERM/SIGINT cancel ctx, which is
	// what unblocks the HTTP server's graceful Shutdown below and, in turn,
	// every defer serve() registers (queue workers, cron scheduler,
	// audit-anchor loop, team backstop) — none of those ran before this
	// change, because the old bare ListenAndServe never returned.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx); err != nil {
		fatalf("serve: %v", err)
	}
}

func serve(ctx context.Context) error {
	cfg, err := loadServerConfig(devMode)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	pool, err := newAppPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	st := store.New(pool)

	kek, err := loadOrGenerateKEK(cfg.KEKPath, devMode)
	if err != nil {
		return fmt.Errorf("load KEK: %w", err)
	}
	keyStore := crypto.NewKeyStore(kek)

	signingKey, err := loadOrGenerateSigningKey(cfg.AuthnSigningKeyPath, devMode)
	if err != nil {
		return fmt.Errorf("load AuthN signing key: %w", err)
	}
	verifier := authn.NewDevVerifier(authn.PublicKey(signingKey))

	prov, err := newProvider()
	if err != nil {
		return fmt.Errorf("configure provider: %w", err)
	}

	// Built ahead of kernel.Kernel below (moved up from its original
	// pre-Tracer position further down this function) — Kernel.Tracer needs
	// this same exporter, not just rest.Server.Exporter, so it has to exist
	// before the Kernel literal does.
	spanExp, shutdownSpanExp, err := newSpanExporter(ctx)
	if err != nil {
		return fmt.Errorf("configure span exporter: %w", err)
	}
	defer shutdownSpanExp()

	// Profiling (docs/observability.md's "profiling" compose profile):
	// on-demand pprof on its OWN loopback-scoped listener (never the mux
	// below /metrics/webhooks share — obs.StartPprofServer's own doc comment
	// says why), plus optional continuous profiling pushed to Grafana
	// Pyroscope. Both are opt-in and no-ops when their env vars are unset —
	// same zero-setup posture as the span exporter above. Mutex/block
	// sampling is a single process-wide rate shared by both consumers
	// (obs.EnableMutexBlockProfiling's own doc comment), so it's set once
	// here regardless of which of the two ends up using it.
	mutexFraction := envIntOr("NEXUS_PPROF_MUTEX_FRACTION", 0)
	blockRate := envIntOr("NEXUS_PPROF_BLOCK_RATE", 0)
	obs.EnableMutexBlockProfiling(mutexFraction, blockRate)

	shutdownPprof, err := obs.StartPprofServer(envOr("NEXUS_PPROF_ADDR", ""))
	if err != nil {
		return fmt.Errorf("start pprof server: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownPprof(shutdownCtx); err != nil {
			log.Error().Err(err).Msg("nexusd: shutdown pprof server")
		}
	}()

	profiler, err := obs.StartPyroscope(obs.PyroscopeConfig{
		ServerAddress:       envOr("NEXUS_PYROSCOPE_ADDR", ""),
		ApplicationName:     "nexusd",
		Tags:                map[string]string{"git_commit": version.GitCommit},
		MutexBlockProfiling: mutexFraction > 0 || blockRate > 0,
	})
	if err != nil {
		return fmt.Errorf("start pyroscope: %w", err)
	}
	if profiler != nil {
		defer func() {
			if err := profiler.Stop(); err != nil {
				log.Error().Err(err).Msg("nexusd: stop pyroscope profiler")
			}
		}()
	}

	// Sign-only audit key custody (README task 5.1): nexusd dials
	// signerd's unix socket and can ask it to sign, never read the key
	// itself — internal/audit/signerkey (the package that CAN read it) is
	// imported only by cmd/signerd, enforced by
	// tests/contract/boundaries_test.go.
	signer := audit.NewSignerClient(cfg.SignerdSocket)
	chain := audit.NewChain(signer)

	// Moved ahead of newToolPipeline (Phase 2's own original ordering had
	// this after) — Phase 11's connectors.Vault needs a live Redis client
	// for its bounded-TTL OAuth state, and newToolPipeline needs the Vault
	// itself to wire platform/connector_fetch and the MCP dynamic resolver.
	redisClient := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})

	vault := &connectors.Vault{Store: st, Keys: keyStore, Providers: newConnectorRegistry(), Redis: redisClient}

	// Built here, ahead of newToolPipeline (moved up from its original
	// Phase 4 position below): Phase 12's platform/retrieve tool needs a
	// Retriever wired into the resident catalog, and a Retriever needs a
	// Gate to reserve embedding calls against (README task 12.4) — the
	// same "construct the shared dependency before the thing that needs
	// it" ordering vault itself already follows one step up.
	gate := cost.NewGate(st, redisClient, cost.DefaultMeters(), cost.GateConfig{})

	delegations := delegate.NewDelegations(st, keyStore, chain)
	teamsSvc := teams.NewService(st, keyStore, chain)
	pipeline, catalog, catalogManifestDigest, admittedSkillBundles, mcpPort := newToolPipeline(st, keyStore, chain, delegations, teamsSvc, vault, gate)
	loadedTools := make([]string, len(catalog))
	for i, c := range catalog {
		loadedTools[i] = c.Name
	}

	approvals := oversight.NewApprovals(st, keyStore, chain)
	inputs := oversight.NewInputs(st, keyStore, chain)
	k := &kernel.Kernel{
		Provider:     provider.Wrap([]provider.Provider{prov}),
		Tools:        kernel.PipelineExecutor{Pipeline: pipeline}, // real tool pipeline, Phase 3
		Budget:       gate,                                        // real reserve-then-reconcile cost gate, Phase 4
		Store:        st,
		Receipts:     chainReceiptFunc(chain),                         // hash-chained audit receipts, Phase 5 task 5.2
		OnSuspend:    onSuspendFunc(approvals),                        // durably record an approval on every suspend, Phase 5 task 5.6
		OnDelegate:   onDelegateFunc(delegations),                     // bind a delegation to its gating tool_use, Phase 8 task 8.10
		Stuck:        reliability.NewRegistry(stuckDetectionWindow),   // README task 6.8
		Tracer:       spanExp,                                         // per-run/per-turn/per-tool-call spans, docs/local-llm.md
		TraceContent: envOr("NEXUS_TRACE_CONTENT", "false") == "true", // opt-in prompt/tool content in traces, docs/local-llm.md — off by default, keeps every span content-free unless explicitly requested
	}

	memStore := &memory.Store{RootDir: envOr("NEXUS_MEMORY_ROOT", ".dev/memory")}

	starter := &kernelRunStarter{
		kernel:      k,
		system:      "You are a helpful agent. Tools may be denied or require approval depending on the session's autonomy level.",
		catalog:     catalog,
		loadedTools: loadedTools,
		maxTurns:    25,
		memory:      memStore,
		store:       st,
	}

	// Wire's own doc comment: must land before the first real dispatch —
	// well before serve() returns and starts accepting traffic. Reuses
	// starter's own System/MaxTurns so a delegated child's kernel run is
	// configured identically to a root run's (README task 8.9's own child
	// session inherits the parent's harness_digest for the same reason).
	delegations.Wire(delegate.Config{
		Kernel: k, Pipeline: pipeline,
		System: starter.system, Catalog: catalog, LoadedTools: loadedTools, MaxTurns: starter.maxTurns,
	})

	resumer := &oversight.Resumer{
		Kernel: k, Approvals: approvals, Store: st, Keys: keyStore,
		System: starter.system, Catalog: catalog, MaxTurns: starter.maxTurns,
	}
	grants := obs.NewGrants(st, keyStore, chain)

	ctl := &runctl.Control{
		Store: st, Keys: keyStore, Chain: chain, Approvals: approvals, Inputs: inputs, Kernel: k,
		System: starter.system, Catalog: catalog, MaxTurns: starter.maxTurns, CatalogManifestDigest: catalogManifestDigest,
	}

	// teamsSvc.Wire's own doc comment: must land before the first real
	// dispatch, the same "well before serve() returns" timing
	// delegations.Wire above already follows. Canceler is ctl itself —
	// endTeam reaps a still-active member the same way runctl.Control.Cancel
	// already reaps anything else (README task 9.9, reusing 8.14).
	teamsSvc.Wire(teams.Config{
		Kernel: k, Pipeline: pipeline, Canceler: ctl,
		System: starter.system, Catalog: catalog, LoadedTools: loadedTools, MaxTurns: starter.maxTurns,
	})

	srv := rest.NewServer(starter, st, keyStore, catalogManifestDigest)
	srv.Verifier = verifier
	srv.Oversight = &nexusdOversightPort{approvals: approvals, resumer: resumer, delegations: delegations, teams: teamsSvc}
	srv.Grants = grants
	srv.ControlPlane = newControlPlane(st, gate, chain, approvals, grants)
	srv.RunCtl = &nexusdRunCtlPort{ctl: ctl}
	srv.Skills = &nexusdSkillSetPort{store: st, bundles: admittedSkillBundles}
	srv.MCP = mcpPort
	srv.Outbox = &surfaces.Outbox{Store: st, Keys: keyStore, Chain: chain}
	srv.OutboxSender = logSender{}

	srv.Exporter = spanExp

	// Live typing-style preview over SSE, decoupled from the durable event
	// path: k.OnChunk fires as the current turn's provider stream decodes
	// each chunk (kernel/turns.go), well before that turn's own
	// accumulation/classification/append — srv.PublishDelta fans it out to
	// whichever clients are subscribed to that session's /v1/runs/{id}/events
	// stream right now, and drops it silently for anyone who isn't (same
	// non-blocking, best-effort delivery the durable path's own
	// broker.publish already has). Nothing about cost reconcile, tool
	// dispatch, or the audit chain reads this — those still only ever see
	// the complete, accumulated turn, exactly as before this was wired.
	k.OnChunk = func(ev kernel.ChunkEvent) {
		switch ev.Chunk.Kind {
		case provider.ChunkContent:
			srv.PublishDelta(ev.SessionID, rest.DeltaDTO{Kind: "content", Text: ev.Chunk.Text})
		case provider.ChunkToolUse:
			srv.PublishDelta(ev.SessionID, rest.DeltaDTO{Kind: "tool_use", ToolUseID: ev.Chunk.ToolUseID, ToolName: ev.Chunk.ToolName})
		case provider.ChunkReasoning:
			// No Text/Opaque on this signal at all (kernel.Kernel.OnChunk's
			// own doc comment already strips it before this closure ever
			// sees it) — a client learns only that the model is reasoning
			// right now, the same "event visible, body redacted" shape
			// EventThought's own durable record has.
			srv.PublishDelta(ev.SessionID, rest.DeltaDTO{Kind: "reasoning"})
		case provider.ChunkUsage, provider.ChunkDone:
			// Turn-level bookkeeping a client has no use for as a live
			// preview — the durable EventTerminal/cost records still carry
			// them.
		}
	}

	stopAnchor := startAnchorLoop(ctx, st, chain)
	defer stopAnchor()

	stopTeamBackstop := startTeamBackstopLoop(ctx, teamsSvc)
	defer stopTeamBackstop()

	stopIdleConversationSweep := startIdleConversationSweepLoop(ctx, ctl)
	defer stopIdleConversationSweep()

	stopWorkers := startQueueWorkers(ctx, st, redisClient, ctl, delegations, teamsSvc)
	defer stopWorkers()

	// Phase 11: four more thin surfaces over the same kernel (README §11) —
	// each gets the SAME session-creation-then-StartRun sequence REST uses,
	// via its own local *StarterAdapter wrapping the one starter every
	// surface shares.
	channels := &MessagingChannels{Store: st, Keys: keyStore}
	outbox := &surfaces.Outbox{Store: st, Keys: keyStore, Chain: chain}

	// One adapter value per surface satisfies both RunStarter and Resumer
	// (surfaces_phase11.go) — passed to both the Starter: and Resume:
	// fields below, so a fresh message and a resumed one drive the SAME
	// underlying *runctl.Control/*kernelRunStarter pair.
	telegramAdapter := telegramStarterAdapter{k: starter, ctl: ctl}
	zaloAdapter := zaloStarterAdapter{k: starter, ctl: ctl}
	emailAdapter := emailStarterAdapter{k: starter, ctl: ctl}

	telegramSrv := &telegram.Server{
		Store: st, KeyStore: keyStore, Starter: telegramAdapter, Resume: telegramAdapter, Channels: channels,
		CatalogManifestDigest: catalogManifestDigest, Outbox: outbox,
		RateLimit: telegram.NewRateLimiter(20, time.Minute),
	}
	zaloSrv := &zalo.Server{
		Store: st, KeyStore: keyStore, Starter: zaloAdapter, Resume: zaloAdapter, Channels: channels,
		CatalogManifestDigest: catalogManifestDigest, Outbox: outbox,
		RateLimit: zalo.NewRateLimiter(20, time.Minute),
	}
	emailSrv := &email.Server{
		Store: st, KeyStore: keyStore, Starter: emailAdapter, Resume: emailAdapter, Channels: channels,
		CatalogManifestDigest: catalogManifestDigest, Outbox: outbox,
		RateLimit: email.NewRateLimiter(20, time.Minute),
	}
	scheduler := &cron.Scheduler{
		Store: st, KeyStore: keyStore, Starter: cronStarterAdapter{k: starter}, Tenants: adminTenantLister{},
		CatalogManifestDigest: catalogManifestDigest,
	}
	go scheduler.Run(ctx)

	mux := http.NewServeMux()
	// /healthz (liveness) and /readyz (Postgres + Redis + signerd all
	// reachable) are unauthenticated and mounted on this OUTER mux — never
	// wrapped by rest.Server's auth middleware (README task 13.1/13.2):
	// a load balancer or Kubernetes probe has no bearer token to send.
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz(pool, redisClient, signer))
	mux.HandleFunc("GET /metrics", handleMetrics(st))
	mux.Handle("/v1/webhooks/telegram/", telegramSrv.Handler())
	mux.Handle("/v1/webhooks/zalo/", zaloSrv.Handler())
	mux.Handle("/v1/webhooks/email/", emailSrv.Handler())
	mux.Handle("/", srv.Handler())

	addr := cfg.HTTPAddr
	fmt.Printf("listening on %s (provider=%s)\n", addr, envOr("NEXUS_PROVIDER", "fake"))

	// README task 13.2 (F2): a real *http.Server with timeouts, driven to a
	// graceful Shutdown by ctx's cancellation (runServe's signal.NotifyContext)
	// instead of the old bare ListenAndServe that never returned.
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
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
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Error().Err(err).Msg("nexusd: graceful shutdown failed")
		}
	}()
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// appPoolMaxConns/appPoolMinConns are README task 13.14's explicit sizing
// (closing production-readiness finding F14: "the connection pool is
// constructed with no config at all"), reconciled against
// deploy/docker-compose.yml's own PgBouncer service (DEFAULT_POOL_SIZE=20,
// MAX_CLIENT_CONN=200) — this is the only client pool that dials THROUGH
// PgBouncer (the admin pools startQueueWorkers/listTenantIDs/etc. construct
// connect directly to Postgres, bypassing it entirely), so its own ceiling
// is what actually has to fit under that budget alongside every other
// process sharing the same PgBouncer instance.
const (
	appPoolMaxConns = 20
	appPoolMinConns = 2
)

// newAppPool builds the one pgxpool.Pool that dials through PgBouncer for
// the life of the process — explicit sizing plus lifecycle knobs, replacing
// the zero-config pgxpool.New serve() used before (README task 13.14).

func newAppPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse pool config: %w", err)
	}
	pcfg.MaxConns = appPoolMaxConns
	pcfg.MinConns = appPoolMinConns
	pcfg.MaxConnLifetime = 30 * time.Minute
	pcfg.MaxConnIdleTime = 5 * time.Minute
	pcfg.HealthCheckPeriod = time.Minute
	return pgxpool.NewWithConfig(ctx, pcfg)
}

// stuckDetectionWindow is internal/reliability.NewRegistry's window (README
// task 6.8): how many recent tool calls a session's own Tracker looks back
// over for a repeating cycle. 8 is small enough to catch a tight retry loop
// within a handful of turns without false-tripping on a normal multi-step
// task that happens to touch the same tool (e.g. several distinct
// file_read calls) more than once.
const stuckDetectionWindow = 8
