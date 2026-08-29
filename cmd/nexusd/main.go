// Command nexusd is the single-binary data+control plane: the kernel loop,
// the harness, and the REST surface, all in one process (see README.md §4).
// Phase 1 gave it two admin subcommands (migrate, seed); Phase 2 adds the
// kernel loop and REST surface, wired together here — this file is the one
// place in the binary allowed to import both kernel/ and
// internal/surfaces/rest (tests/contract/boundaries_test.go forbids the
// surface from importing the kernel directly; the composition root is
// exempt because it isn't either of those packages).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/anthropic"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/version"
	"github.com/truongpx396/nexus-agent-demo/kernel"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

const (
	// defaultMigrateDSN connects DIRECTLY to Postgres (bypassing PgBouncer):
	// migrations are an admin operation, not a tenant-scoped one, and DDL
	// has no reason to go through the transaction-pooling tier the runtime
	// path depends on.
	defaultMigrateDSN = "postgres://nexus:nexus@localhost:5433/nexus"
	// defaultAppDSN connects THROUGH PgBouncer in transaction-pooling
	// mode, as nexus_app — an ordinary role with no RLS bypass (migrations/
	// 0000_app_role.sql). nexus, the migration role, is a superuser and
	// would silently see every tenant's rows regardless of RLS.
	defaultAppDSN = "postgres://nexus_app:nexus_app@localhost:6432/nexus"
)

func main() {
	ctx := context.Background()

	if len(os.Args) < 2 {
		runServe()
		return
	}
	switch os.Args[1] {
	case "migrate":
		if err := runMigrate(ctx); err != nil {
			fatalf("migrate: %v", err)
		}
	case "seed":
		if err := runSeed(ctx, os.Args[2:]); err != nil {
			fatalf("seed: %v", err)
		}
	default:
		runServe()
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// defaultKEKPath is a local, gitignored dev key (see .gitignore's
// /.dev/ entry) — a production deployment sources the KEK from an external
// vault/HSM behind the same internal/crypto.KEK type (internal/crypto's doc
// comment). Generated on first run so `make up && make run` works with zero
// setup.
const defaultKEKPath = ".dev/kek.key"

func runServe() {
	fmt.Printf("nexusd %s (%s)\n", version.Version, version.GitCommit)

	ctx := context.Background()
	if err := serve(ctx); err != nil {
		fatalf("serve: %v", err)
	}
}

func serve(ctx context.Context) error {
	dsn := envOr("NEXUS_DATABASE_URL", defaultAppDSN)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	st := store.New(pool)

	kek, err := loadOrGenerateKEK(envOr("NEXUS_KEK_PATH", defaultKEKPath))
	if err != nil {
		return fmt.Errorf("load KEK: %w", err)
	}
	keyStore := crypto.NewKeyStore(kek)

	prov, err := newProvider()
	if err != nil {
		return fmt.Errorf("configure provider: %w", err)
	}

	starter := &kernelRunStarter{
		kernel: &kernel.Kernel{
			Provider: provider.Wrap([]provider.Provider{prov}),
			Tools:    kernel.NotImplementedToolExecutor{}, // real tool pipeline lands Phase 3
			Budget:   kernel.NoopBudgetGate{},             // real cost gate lands Phase 4
			Store:    st,
		},
		system:   "You are a helpful agent.",
		maxTurns: 25,
	}

	srv := rest.NewServer(starter, st, keyStore)
	addr := envOr("NEXUS_HTTP_ADDR", ":8080")
	fmt.Printf("listening on %s (provider=%s)\n", addr, envOr("NEXUS_PROVIDER", "fake"))
	return http.ListenAndServe(addr, srv.Handler()) //nolint:gosec // dev/demo server; timeouts are a hardening task, not a Phase 2 one
}

// newProvider picks the fake (default, no credentials needed — the demo
// command in README.md §5 must work with zero setup) or the real Anthropic
// adapter, never both: correctness tests always run against
// internal/provider/fake regardless of this switch (constitution Principle
// IX) — this only controls what a live `nexusd run` talks to.
func newProvider() (provider.Provider, error) {
	switch envOr("NEXUS_PROVIDER", "fake") {
	case "anthropic":
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("NEXUS_PROVIDER=anthropic requires ANTHROPIC_API_KEY")
		}
		model := envOr("NEXUS_ANTHROPIC_MODEL", "claude-sonnet-5")
		return anthropic.New(apiKey, model), nil
	case "fake":
		// An echo-style script: enough to drive a real turn through the
		// loop without a live model. Real scripted corpora live in
		// evals/corpus/ and internal/provider/fake's own tests; this is
		// just what an unscripted `nexusd run` demo has to say.
		return fake.New(fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "content", Text: "Hello from the Phase 2 kernel loop demo."},
			{Kind: "done", Done: "stop"},
		}}), nil
	default:
		return nil, fmt.Errorf("unknown NEXUS_PROVIDER %q (want fake or anthropic)", os.Getenv("NEXUS_PROVIDER"))
	}
}

func loadOrGenerateKEK(path string) (crypto.KEK, error) {
	f, err := os.Open(path) //nolint:gosec // path is an operator-controlled config value (NEXUS_KEK_PATH), never request input
	if err == nil {
		defer f.Close() //nolint:errcheck // read-only handle; nothing to flush
		return crypto.LoadKEK(f)
	}
	if !os.IsNotExist(err) {
		return crypto.KEK{}, fmt.Errorf("open KEK file %s: %w", path, err)
	}

	kek, err := crypto.GenerateKEK()
	if err != nil {
		return crypto.KEK{}, fmt.Errorf("generate KEK: %w", err)
	}
	if err := os.MkdirAll(".dev", 0o700); err != nil {
		return crypto.KEK{}, fmt.Errorf("create .dev: %w", err)
	}
	if err := os.WriteFile(path, kek.Bytes(), 0o600); err != nil {
		return crypto.KEK{}, fmt.Errorf("write KEK file %s: %w", path, err)
	}
	slog.Info("generated a new dev KEK", "path", path)
	return kek, nil
}

// kernelRunStarter is the only implementation of rest.RunStarter this binary
// ships, and the only place a kernel.RunState/kernel.RunConfig gets built —
// internal/surfaces/rest never constructs either directly (starter.go's doc
// comment).
type kernelRunStarter struct {
	kernel   *kernel.Kernel
	system   string
	catalog  []provider.ToolSchema
	maxTurns int
}

func (a *kernelRunStarter) StartRun(_ context.Context, req rest.RunRequest) (<-chan rest.RunEvent, error) {
	st := &kernel.RunState{
		TenantID:  req.TenantID,
		SessionID: req.SessionID,
		Seal:      kernel.SealFunc(req.Seal),
	}
	cfg := kernel.RunConfig{
		System:   a.system,
		Catalog:  a.catalog,
		ModelID:  req.ModelID,
		MaxTurns: a.maxTurns,
		Input:    req.Input,
	}

	ch := make(chan rest.RunEvent, 8)
	go func() {
		defer close(ch)
		// A run outlives the HTTP request that started it — the client
		// already has its 202 and may not even open the SSE stream.
		for ev, err := range a.kernel.Run(context.Background(), st, cfg) {
			ch <- rest.RunEvent{Event: ev, Err: err}
			if err != nil {
				return
			}
		}
	}()
	return ch, nil
}

func runMigrate(ctx context.Context) error {
	dsn := envOr("NEXUS_MIGRATE_DATABASE_URL", defaultMigrateDSN)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	applied, err := store.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	for _, a := range applied {
		fmt.Printf("applied %s\n", a.Version)
	}

	enabled, total, err := store.RLSTableCount(ctx, pool)
	if err != nil {
		return fmt.Errorf("count RLS tables: %w", err)
	}
	fmt.Printf("RLS enabled on %d/%d tenant tables; tenant scope is transaction-local\n", enabled, total)
	return nil
}

func runSeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	tenantName := fs.String("tenant", "acme", "tenant name to seed")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dsn := envOr("NEXUS_DATABASE_URL", defaultAppDSN)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	// Deterministic from the name, so `make seed TENANT=acme` is idempotent
	// across runs rather than minting a new tenant every time.
	tenantID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("nexus-agent-demo/tenant/"+*tenantName))

	s := store.New(pool)
	err = s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenants (tenant_id, name) VALUES ($1, $2)
			 ON CONFLICT (tenant_id) DO NOTHING`,
			tenantID, *tenantName,
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("seed tenant: %w", err)
	}

	fmt.Printf("seeded tenant %q (tenant_id=%s)\n", *tenantName, tenantID)
	return nil
}
