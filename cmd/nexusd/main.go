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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/hooks"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions/safety"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/anthropic"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
	"github.com/truongpx396/nexus-agent-demo/internal/tools/builtin"
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
	// defaultRedisAddr matches deploy/docker-compose.yml's host mapping
	// (6380, not Redis's usual 6379 — see that file's own comment on why).
	// internal/cost.Gate uses it for the tenant-ceiling epoch-marked
	// counter (README task 4.4); nothing else in this binary touches Redis.
	defaultRedisAddr = "localhost:6380"
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

	pipeline, catalog, catalogManifestDigest := newToolPipeline()
	loadedTools := make([]string, len(catalog))
	for i, c := range catalog {
		loadedTools[i] = c.Name
	}

	redisClient := redis.NewClient(&redis.Options{Addr: envOr("NEXUS_REDIS_ADDR", defaultRedisAddr)})
	gate := cost.NewGate(st, redisClient, cost.DefaultMeters(), cost.GateConfig{})

	starter := &kernelRunStarter{
		kernel: &kernel.Kernel{
			Provider: provider.Wrap([]provider.Provider{prov}),
			Tools:    kernel.PipelineExecutor{Pipeline: pipeline}, // real tool pipeline, Phase 3
			Budget:   gate,                                        // real reserve-then-reconcile cost gate, Phase 4
			Store:    st,
		},
		system:      "You are a helpful agent. Tools may be denied or require approval depending on the session's autonomy level.",
		catalog:     catalog,
		loadedTools: loadedTools,
		maxTurns:    25,
	}

	srv := rest.NewServer(starter, st, keyStore, catalogManifestDigest)
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
			{Kind: "usage", InputUncached: 120, OutputTokens: 18},
			{Kind: "done", Done: "stop"},
		}}), nil
	default:
		return nil, fmt.Errorf("unknown NEXUS_PROVIDER %q (want fake or anthropic)", os.Getenv("NEXUS_PROVIDER"))
	}
}

// newToolPipeline wires the Phase 3 tool pipeline: the resident catalog
// (the five builtin tools), the permission chain's tenant-independent
// config, and the hook dispatcher. There is no tenant config store yet
// (that's Phase 7's internal/config) — one process-wide profile bound to
// every builtin tool is the honest Phase 3 stand-in, the same way Phase 2
// pinned one hardcoded system prompt.
func newToolPipeline() (*tools.Pipeline, []provider.ToolSchema, []byte) {
	reg := tools.NewRegistry()
	if err := reg.DeclareNamespace("platform", "nexusd"); err != nil {
		fatalf("declare platform namespace: %v", err)
	}

	builtinTools := []tools.Tool{
		builtin.FileRead{},
		builtin.FileWrite{},
		builtin.FileSearch{},
		builtin.Shell{},
		builtin.WebFetch{},
	}
	var toolRefs []string
	var catalog []provider.ToolSchema
	for _, t := range builtinTools {
		if err := reg.Register(t); err != nil {
			fatalf("register tool %s: %v", t.ID(), err)
		}
		status, findings := tools.Scan(t.Descriptor())
		if err := reg.SetAdmissionStatus(t.ID(), status); err != nil {
			fatalf("set admission status for %s: %v", t.ID(), err)
		}
		if status != tools.AdmissionClean {
			fatalf("builtin tool %s failed admission (%s): %v", t.ID(), status, findings)
		}
		toolRefs = append(toolRefs, t.ID().String())
		d := t.Descriptor()
		catalog = append(catalog, provider.ToolSchema{Name: d.ID.String(), Description: d.Description, InputSchema: d.InputSchema})
	}

	manifest := tools.BuildManifest(reg)
	chain := permissions.NewChain(permissions.ChainConfig{
		Profiles: permissions.ProfileSet{Profiles: []permissions.ToolProfile{permissions.NewToolProfile("default", 1, toolRefs...)}},
		Safety:   safety.NewClassifier(safety.DefaultRules(), demoSafetyModel{}, 0),
	})

	pipeline := tools.NewPipeline(tools.PipelineConfig{
		Registry:      reg,
		Manifest:      manifest,
		Chain:         chain,
		Hooks:         hooks.NewDispatcher(),
		Blobs:         tools.BlobStore{Dir: envOr("NEXUS_BLOB_DIR", ".dev/blobs")},
		WorkspaceRoot: envOr("NEXUS_WORKSPACE_ROOT", ".dev/workspaces"),
	})
	return pipeline, catalog, manifest.Digest
}

// demoSafetyModel stands in for Gate 3's model leg (internal/permissions/
// safety.ModelClassifier): Phase 3 ships no real model-backed classifier —
// wiring one through the ordinary Provider port is future work, not a
// Phase 3 task — so this always defers instead of asking about every call
// safety.DefaultRules doesn't recognize, which would otherwise swamp the
// "governed agent" demo in approval requests for plainly harmless calls.
type demoSafetyModel struct{}

func (demoSafetyModel) Classify(context.Context, string, string) (safety.Verdict, string, error) {
	return safety.VerdictDefer, "no real safety model configured (Phase 3 demo default)", nil
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
	kernel      *kernel.Kernel
	system      string
	catalog     []provider.ToolSchema
	loadedTools []string
	maxTurns    int
}

func (a *kernelRunStarter) StartRun(_ context.Context, req rest.RunRequest) (<-chan rest.RunEvent, error) {
	st := &kernel.RunState{
		TenantID:  req.TenantID,
		SessionID: req.SessionID,
		Seal:      kernel.SealFunc(req.Seal),
	}
	cfg := kernel.RunConfig{
		System:        a.system,
		Catalog:       a.catalog,
		LoadedTools:   a.loadedTools,
		ModelID:       req.ModelID,
		MaxTurns:      a.maxTurns,
		Input:         req.Input,
		AutonomyLevel: req.AutonomyLevel,
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

	if err := s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return seedPriceBook(ctx, tx, tenantID)
	}); err != nil {
		return fmt.Errorf("seed price book: %w", err)
	}

	fmt.Printf("seeded tenant %q (tenant_id=%s)\n", *tenantName, tenantID)
	return nil
}

// seedPriceBook inserts one price book entry per token meter (README task
// 4.3) the first time a tenant is seeded — cost governance is otherwise
// inert (internal/cost.Gate fails closed with "no price book entry" on
// every Reserve, on purpose: an unpriced meter must never look free).
// Idempotent like the tenant insert above it: a second `make seed` for the
// same tenant is a no-op once MeterOutput's wildcard entry already exists.
// Prices are illustrative, Claude-Sonnet-class figures per million tokens;
// every entry uses cost.WildcardSubject, so one price covers every model
// this demo routes to — a real per-model override is a feature the price
// book schema supports (internal/cost/pricebook.go) but this seed step
// doesn't exercise.
func seedPriceBook(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	existing, err := cost.LoadPriceBook(ctx, tx)
	if err != nil {
		return err
	}
	if _, ok := existing.Lookup(cost.MeterOutput, cost.WildcardSubject, time.Now()); ok {
		return nil
	}

	now := time.Now()
	for _, p := range []struct {
		meter                 cost.MeterID
		pricePerMillionMicros int64
	}{
		{cost.MeterInputUncached, 3_000_000},   // $3 / million input tokens
		{cost.MeterInputCacheRead, 300_000},    // $0.30 / million cache-read tokens
		{cost.MeterInputCacheWrite, 3_750_000}, // $3.75 / million cache-write tokens
		{cost.MeterOutput, 15_000_000},         // $15 / million output tokens
	} {
		if err := cost.InsertPriceBookEntry(ctx, tx, tenantID, cost.PriceBookEntry{
			Meter: p.meter, Subject: cost.WildcardSubject, Version: 1,
			Currency: cost.DefaultCurrency, PricePerMillionMicros: p.pricePerMillionMicros, EffectiveFrom: now,
		}); err != nil {
			return err
		}
	}
	return nil
}
