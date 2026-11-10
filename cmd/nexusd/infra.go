package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	auditpg "github.com/truongpx396/nexus-agent-demo/internal/audit/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/authn"
	cryptopg "github.com/truongpx396/nexus-agent-demo/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/obs/profiling"
	queueredis "github.com/truongpx396/nexus-agent-demo/internal/queue/redis"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/version"
)

// infra is what serve opens before it builds any feature: the connections and
// key material every feature shares.
type infra struct {
	pool     *pgxpool.Pool
	st       *store.Store
	redis    *redis.Client
	keys     *cryptopg.KeyStore
	chain    *auditpg.Chain
	signer   *audit.SignerClient
	verifier *authn.DevVerifier
	queue    *queueredis.Streams
}

// openInfra connects to Postgres (through PgBouncer) and Redis and loads the key
// material. On failure it releases whatever it had opened; on success the
// caller owns the result and defers close.
func openInfra(ctx context.Context, cfg serverConfig) (_ *infra, err error) {
	pool, err := newAppPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	in := &infra{pool: pool, st: store.New(pool)}
	defer func() {
		if err != nil {
			in.close()
		}
	}()

	kek, err := loadOrGenerateKEK(cfg.KEKPath, devMode)
	if err != nil {
		return nil, fmt.Errorf("load KEK: %w", err)
	}
	in.keys = cryptopg.NewKeyStore(kek)

	signingKey, err := loadOrGenerateSigningKey(cfg.AuthnSigningKeyPath, devMode)
	if err != nil {
		return nil, fmt.Errorf("load AuthN signing key: %w", err)
	}
	in.verifier = authn.NewDevVerifier(authn.PublicKey(signingKey))

	// Sign-only audit key custody (README task 5.1): nexusd dials signerd's unix
	// socket and can ask it to sign, never read the key itself —
	// internal/audit/signerkey (the package that CAN read it) is imported only by
	// cmd/signerd, enforced by tests/contract/boundaries_test.go.
	in.signer = audit.NewSignerClient(cfg.SignerdSocket)
	in.chain = auditpg.NewChain(in.signer)

	in.redis = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})

	// internal/queue's Redis Streams adapter (the retired Postgres queue_jobs
	// table's successor): the run starter and run control both enqueue through it
	// once they have durably seeded or appended what the job continues.
	in.queue, err = queueredis.NewStreams(ctx, in.redis)
	if err != nil {
		return nil, fmt.Errorf("configure queue: %w", err)
	}
	return in, nil
}

// close releases the Redis client and the pool. Call it after everything built
// over them has stopped, so no worker is mid-query when the pool goes away.
func (in *infra) close() {
	if in.redis != nil {
		if err := in.redis.Close(); err != nil {
			log.Error().Err(err).Msg("nexusd: close redis client")
		}
	}
	in.pool.Close()
}

// appPoolMaxConns/appPoolMinConns are README task 13.14's explicit sizing
// (closing production-readiness finding F14: "the connection pool is
// constructed with no config at all"), reconciled against
// deploy/docker-compose.yml's own PgBouncer service (DEFAULT_POOL_SIZE=20,
// MAX_CLIENT_CONN=200) — this is the only client pool that dials THROUGH
// PgBouncer (the admin pools startQueueWorkers/tenantDirectory/etc. construct
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

// startProfiling starts the opt-in profilers (docs/observability.md's
// "profiling" compose profile): on-demand pprof on its OWN loopback-scoped
// listener (never the mux /metrics and the webhooks share —
// profiling.StartPprofServer's doc comment says why), plus optional continuous
// profiling pushed to Grafana Pyroscope. Both are no-ops when their env vars are
// unset, the same zero-setup posture as the span exporter. Mutex/block sampling
// is one process-wide rate shared by both consumers
// (profiling.EnableMutexBlockProfiling's doc comment), so it is set once here
// whichever of the two ends up using it.
func startProfiling() (stop func(), err error) {
	mutexFraction := envIntOr("NEXUS_PPROF_MUTEX_FRACTION", 0)
	blockRate := envIntOr("NEXUS_PPROF_BLOCK_RATE", 0)
	profiling.EnableMutexBlockProfiling(mutexFraction, blockRate)

	shutdownPprof, err := profiling.StartPprofServer(envOr("NEXUS_PPROF_ADDR", ""))
	if err != nil {
		return nil, fmt.Errorf("start pprof server: %w", err)
	}
	stopPprof := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownPprof(ctx); err != nil {
			log.Error().Err(err).Msg("nexusd: shutdown pprof server")
		}
	}

	profiler, err := profiling.StartPyroscope(profiling.PyroscopeConfig{
		ServerAddress:       envOr("NEXUS_PYROSCOPE_ADDR", ""),
		ApplicationName:     "nexusd",
		Tags:                map[string]string{"git_commit": version.GitCommit},
		MutexBlockProfiling: mutexFraction > 0 || blockRate > 0,
	})
	if err != nil {
		stopPprof() // already listening
		return nil, fmt.Errorf("start pyroscope: %w", err)
	}
	return func() {
		if profiler != nil {
			if err := profiler.Stop(); err != nil {
				log.Error().Err(err).Msg("nexusd: stop pyroscope profiler")
			}
		}
		stopPprof()
	}, nil
}
