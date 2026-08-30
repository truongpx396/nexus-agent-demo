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
	"encoding/json"
	"flag"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/hooks"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions/safety"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/anthropic"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/sandbox"
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
	// defaultSignerdSocket matches cmd/signerd's own default — nexusd
	// dials it as a Signer client (internal/audit.SignerClient); it never
	// reads the private key itself (README task 5.1,
	// tests/contract/boundaries_test.go's dedicated rule).
	defaultSignerdSocket = ".dev/signerd.sock"
	// anchorInterval is how often the periodic anchor+verify pass (task
	// 5.3's "scheduled verifier") runs against every tenant.
	anchorInterval = 5 * time.Minute
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
	case "verify-chain":
		if err := runVerifyChain(ctx, os.Args[2:]); err != nil {
			fatalf("verify-chain: %v", err)
		}
	case "erase":
		if err := runErase(ctx, os.Args[2:]); err != nil {
			fatalf("erase: %v", err)
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

	// Sign-only audit key custody (README task 5.1): nexusd dials
	// signerd's unix socket and can ask it to sign, never read the key
	// itself — internal/audit/signerkey (the package that CAN read it) is
	// imported only by cmd/signerd, enforced by
	// tests/contract/boundaries_test.go.
	signer := audit.NewSignerClient(envOr("NEXUS_SIGNERD_SOCKET", defaultSignerdSocket))
	chain := audit.NewChain(signer)

	pipeline, catalog, catalogManifestDigest := newToolPipeline(st, keyStore, chain)
	loadedTools := make([]string, len(catalog))
	for i, c := range catalog {
		loadedTools[i] = c.Name
	}

	redisClient := redis.NewClient(&redis.Options{Addr: envOr("NEXUS_REDIS_ADDR", defaultRedisAddr)})
	gate := cost.NewGate(st, redisClient, cost.DefaultMeters(), cost.GateConfig{})

	approvals := oversight.NewApprovals(st, keyStore, chain)
	k := &kernel.Kernel{
		Provider:  provider.Wrap([]provider.Provider{prov}),
		Tools:     kernel.PipelineExecutor{Pipeline: pipeline}, // real tool pipeline, Phase 3
		Budget:    gate,                                        // real reserve-then-reconcile cost gate, Phase 4
		Store:     st,
		Receipts:  chainReceiptFunc(chain),  // hash-chained audit receipts, Phase 5 task 5.2
		OnSuspend: onSuspendFunc(approvals), // durably record an approval on every suspend, Phase 5 task 5.6
	}

	starter := &kernelRunStarter{
		kernel:      k,
		system:      "You are a helpful agent. Tools may be denied or require approval depending on the session's autonomy level.",
		catalog:     catalog,
		loadedTools: loadedTools,
		maxTurns:    25,
	}

	resumer := &oversight.Resumer{
		Kernel: k, Approvals: approvals, Store: st, Keys: keyStore,
		System: starter.system, Catalog: catalog, MaxTurns: starter.maxTurns,
	}
	grants := obs.NewGrants(st, keyStore, chain)

	srv := rest.NewServer(starter, st, keyStore, catalogManifestDigest)
	srv.Oversight = &nexusdOversightPort{approvals: approvals, resumer: resumer}
	srv.Grants = grants

	stopAnchor := startAnchorLoop(ctx, st, chain)
	defer stopAnchor()

	addr := envOr("NEXUS_HTTP_ADDR", ":8080")
	fmt.Printf("listening on %s (provider=%s)\n", addr, envOr("NEXUS_PROVIDER", "fake"))
	return http.ListenAndServe(addr, srv.Handler()) //nolint:gosec // dev/demo server; timeouts are a hardening task, not a Phase 2 one
}

// chainReceiptFunc adapts internal/audit.Chain.Append to kernel.ReceiptFunc
// — the seam kernel/types.go declares locally so kernel itself never
// imports internal/audit (not on its own import allowlist).
func chainReceiptFunc(chain *audit.Chain) kernel.ReceiptFunc {
	return func(ctx context.Context, tx pgx.Tx, e store.Event) error {
		_, err := chain.Append(ctx, tx, e.TenantID, e.SessionID, e.Seq, e.EventID, string(e.Type), e.PayloadDigest)
		return err
	}
}

// onSuspendFunc adapts oversight.Approvals.Create to kernel.OnSuspend —
// rendering the ContextPackage a human approver sees (README task 5.6:
// "never a bare UUID") from the tool_use's own tool_id/effect_class/
// plaintext input, all of which kernel.SuspendRequest already carries
// (tools/pipeline.go's Ask branch is where EffectClass and CanonicalDigest
// both originate, in the one place that already has the descriptor).
func onSuspendFunc(approvals *oversight.Approvals) kernel.OnSuspend {
	return func(ctx context.Context, tx pgx.Tx, req kernel.SuspendRequest) error {
		_, err := approvals.Create(ctx, oversight.CreateApprovalRequest{
			TenantID: req.TenantID, SessionID: req.SessionID, ToolUseEventID: req.ToolUseEventID,
			ToolID: req.ToolID, AskKind: req.AskKind, CanonicalDigest: req.CanonicalDigest,
			Context: oversight.ContextPackage{ToolID: req.ToolID, EffectClass: req.EffectClass, Input: req.Input},
		})
		return err
	}
}

// startAnchorLoop runs the scheduled verifier task 5.3 asks for: every
// anchorInterval, anchor every tenant's new receipts and verify the whole
// chain, logging (not panicking on) a break or a gap — an alert a real
// deployment would ship to its paging system, not a reason to crash the
// process that is the source of truth for the very thing it's checking.
func startAnchorLoop(ctx context.Context, st *store.Store, chain *audit.Chain) (stop func()) {
	ticker := time.NewTicker(anchorInterval)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				anchorAndVerifyAllTenants(ctx, st, chain)
			}
		}
	}()
	return func() { close(done) }
}

func anchorAndVerifyAllTenants(ctx context.Context, st *store.Store, chain *audit.Chain) {
	tenantIDs, err := listTenantIDs(ctx)
	if err != nil {
		slog.Error("nexusd: list tenants for anchor/verify pass", "error", err)
		return
	}
	for _, tenantID := range tenantIDs {
		if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, _, err := chain.Anchor(ctx, tx, tenantID); err != nil {
				return err
			}
			report, err := chain.Verify(ctx, tx, tenantID)
			if err != nil {
				return err
			}
			if !report.OK() {
				slog.Error("nexusd: audit chain verification found a problem", "tenant_id", tenantID, "breaks", report.Breaks, "gaps", report.Gaps)
			}
			return nil
		}); err != nil {
			slog.Error("nexusd: anchor/verify pass failed", "tenant_id", tenantID, "error", err)
		}
	}
}

// listTenantIDs enumerates every tenant in the system — genuinely
// cross-tenant, unlike everything else in this binary (store.Store.
// InTenantTx is "the only sanctioned way to scope a database operation to
// a tenant," per its own doc comment, and has no notion of "every
// tenant"). st's own pool connects as nexus_app through PgBouncer, an
// ordinary role RLS-restricted to whatever app.tenant_id the CURRENT
// transaction set — which is nothing, here, on purpose: there is no tenant
// to scope to yet, that's the whole point of this query. So this one admin
// operation connects directly as the migration superuser instead, exactly
// like runMigrate already does for admin-only DDL — the anchor/verify pass
// (README task 5.3) is the same kind of platform-operator action, not a
// tenant-scoped one.
func listTenantIDs(ctx context.Context) ([]uuid.UUID, error) {
	dsn := envOr("NEXUS_ADMIN_DATABASE_URL", envOr("NEXUS_MIGRATE_DATABASE_URL", defaultMigrateDSN))
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect as admin: %w", err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, `SELECT tenant_id FROM tenants`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// nexusdOversightPort is the only implementation of rest.OversightPort this
// binary ships — the seam that lets internal/surfaces/rest drive
// internal/oversight (which itself imports kernel for Kernel.Resume)
// without importing it directly, exactly mirroring kernelRunStarter's own
// role for kernel.Kernel.Run.
type nexusdOversightPort struct {
	approvals *oversight.Approvals
	resumer   *oversight.Resumer
}

func (p *nexusdOversightPort) GetApproval(ctx context.Context, tenantID, approvalID uuid.UUID) (rest.ApprovalView, error) {
	ap, err := p.approvals.Get(ctx, tenantID, approvalID)
	if err != nil {
		return rest.ApprovalView{}, err
	}
	return toApprovalView(ap), nil
}

func (p *nexusdOversightPort) ListPendingApprovals(ctx context.Context, tenantID uuid.UUID) ([]rest.ApprovalView, error) {
	aps, err := p.approvals.ListPending(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	views := make([]rest.ApprovalView, len(aps))
	for i, ap := range aps {
		views[i] = toApprovalView(ap)
	}
	return views, nil
}

func toApprovalView(ap oversight.Approval) rest.ApprovalView {
	ctxJSON, _ := json.Marshal(ap.Context) //nolint:errcheck // ContextPackage is always marshalable (json.RawMessage + strings)
	return rest.ApprovalView{
		ApprovalID: ap.ApprovalID.String(), SessionID: ap.SessionID.String(), ToolID: ap.ToolID,
		AskKind: ap.AskKind, Status: string(ap.Status), Context: ctxJSON, ExpiresAt: ap.ExpiresAt, CreatedAt: ap.CreatedAt,
	}
}

func (p *nexusdOversightPort) Grant(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string) rest.ResumeOutcome {
	return drainResume(p.resumer.Grant(ctx, tenantID, approvalID, decidedBy))
}

func (p *nexusdOversightPort) GrantModified(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string, modifiedInput json.RawMessage) rest.ResumeOutcome {
	return drainResume(p.resumer.GrantModified(ctx, tenantID, approvalID, decidedBy, modifiedInput))
}

func (p *nexusdOversightPort) Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) rest.ResumeOutcome {
	return drainResume(p.resumer.Deny(ctx, tenantID, approvalID, decidedBy, reason))
}

// drainResume fully consumes a Kernel.Resume generator — the same
// drain-into-a-summary kernelRunStarter's own goroutine does for
// Kernel.Run, except synchronous: an approval decision's HTTP response
// waits for the resumed run to finish (or suspend again), rather than
// handing back a channel the way a fresh run's 202 does. See rest.
// ResumeOutcome's own doc comment for why that's an acceptable trade for
// this endpoint.
func drainResume(events iter.Seq2[store.Event, error]) rest.ResumeOutcome {
	var out rest.ResumeOutcome
	for ev, err := range events {
		if err != nil {
			out.Err = err.Error()
			return out
		}
		out.SessionID = ev.SessionID.String()
		out.EventsAppended++
	}
	return out
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
// pinned one hardcoded system prompt. st/keyStore/chain wire Phase 5's
// derived-artifact tracking (task 5.4) into BudgetResult's spill path.
func newToolPipeline(st *store.Store, keyStore *crypto.KeyStore, chain *audit.Chain) (*tools.Pipeline, []provider.ToolSchema, []byte) {
	reg := tools.NewRegistry()
	if err := reg.DeclareNamespace("platform", "nexusd"); err != nil {
		fatalf("declare platform namespace: %v", err)
	}

	webFetchAllowlist := strings.Split(envOr("NEXUS_WEB_FETCH_ALLOWLIST", ""), ",")

	builtinTools := []tools.Tool{
		builtin.FileRead{},
		builtin.FileWrite{},
		builtin.FileSearch{},
		builtin.Shell{},
		builtin.WebFetch{AllowedHosts: webFetchAllowlist},
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
	permChain := permissions.NewChain(permissions.ChainConfig{
		Profiles: permissions.ProfileSet{Profiles: []permissions.ToolProfile{permissions.NewToolProfile("default", 1, toolRefs...)}},
		Safety:   safety.NewClassifier(safety.DefaultRules(), demoSafetyModel{}, 0),
	})

	workspaceRoot := envOr("NEXUS_WORKSPACE_ROOT", ".dev/workspaces")
	pipeline := tools.NewPipeline(tools.PipelineConfig{
		Registry:         reg,
		Manifest:         manifest,
		Chain:            permChain,
		Hooks:            hooks.NewDispatcher(),
		Blobs:            tools.BlobStore{Dir: envOr("NEXUS_BLOB_DIR", ".dev/blobs")},
		WorkspaceRoot:    workspaceRoot,
		DerivedArtifacts: derivedArtifactRecorder(st, chain),
		SandboxFactory:   newSandboxFactory(workspaceRoot),
	})
	return pipeline, catalog, manifest.Digest
}

// derivedArtifactRecorder wires internal/tools.DerivedArtifactRecorder to a
// durable derived_artifacts row (README task 5.4) — best-effort, its own
// small transaction (not the caller's): internal/crypto/shred.go's
// ReconcileDerivedArtifacts is the backstop for whatever this misses, per
// its own doc comment.
func derivedArtifactRecorder(st *store.Store, chain *audit.Chain) tools.DerivedArtifactRecorder {
	_ = chain // reserved: a future phase may want a receipt per spill too; not required by task 5.4 itself
	return func(ctx context.Context, tenantID, sessionID uuid.UUID, kind, path string) error {
		return st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO derived_artifacts (artifact_id, tenant_id, session_id, kind, path) VALUES ($1,$2,$3,$4,$5)`,
				uuid.New(), tenantID, sessionID, kind, path,
			)
			return err
		})
	}
}

// newSandboxFactory wires platform/shell to run inside Docker (README task
// 5.12) when NEXUS_SANDBOX=docker and a daemon is actually reachable —
// opt-in, not the default: this demo's zero-setup path (`make up && make
// run`) must keep working on a machine with no Docker daemon running,
// exactly like NEXUS_PROVIDER=fake needs no ANTHROPIC_API_KEY.
func newSandboxFactory(workspaceRoot string) func(uuid.UUID) tools.SandboxExec {
	if envOr("NEXUS_SANDBOX", "") != "docker" {
		return nil
	}
	docker, err := sandbox.NewDocker()
	if err != nil {
		slog.Warn("nexusd: NEXUS_SANDBOX=docker but connecting to Docker failed; platform/shell stays unsandboxed", "error", err)
		return nil
	}
	return func(sessionID uuid.UUID) tools.SandboxExec {
		return sandbox.SessionSandbox{
			Docker: docker,
			Config: sandbox.Config{WorkspaceDir: filepath.Join(workspaceRoot, sessionID.String())},
		}
	}
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

// runVerifyChain is `nexusd verify-chain` (the Makefile's own stubbed
// target, README task 5.3): anchor and verify every tenant's audit chain
// once, printing a clean/broken report per tenant instead of the periodic
// background pass startAnchorLoop runs inside `serve`.
func runVerifyChain(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify-chain", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	dsn := envOr("NEXUS_DATABASE_URL", defaultAppDSN)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	st := store.New(pool)

	signer := audit.NewSignerClient(envOr("NEXUS_SIGNERD_SOCKET", defaultSignerdSocket))
	chain := audit.NewChain(signer)

	tenantIDs, err := listTenantIDs(ctx)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}

	anyBroken := false
	for _, tenantID := range tenantIDs {
		var report audit.Report
		err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, _, err := chain.Anchor(ctx, tx, tenantID); err != nil {
				return err
			}
			var verr error
			report, verr = chain.Verify(ctx, tx, tenantID)
			return verr
		})
		if err != nil {
			return fmt.Errorf("verify tenant %s: %w", tenantID, err)
		}
		if report.OK() {
			fmt.Printf("tenant %s: OK (%d receipts, %d anchors)\n", tenantID, report.ReceiptsChecked, report.AnchorsChecked)
			continue
		}
		anyBroken = true
		fmt.Printf("tenant %s: BROKEN — %d break(s), %d gap(s)\n", tenantID, len(report.Breaks), len(report.Gaps))
		for _, b := range report.Breaks {
			fmt.Printf("  break: session=%s seq=%d kind=%s detail=%s\n", b.SessionID, b.Seq, b.Kind, b.Detail)
		}
		for _, g := range report.Gaps {
			fmt.Printf("  gap: session=%s missing_seq=%d\n", g.SessionID, g.MissingSeq)
		}
	}
	if anyBroken {
		return fmt.Errorf("audit chain verification found a break or gap")
	}
	return nil
}

// runErase is `nexusd erase --tenant=name` (or --session=<uuid>): the
// admin operation behind internal/crypto/shred.go's erasure transaction
// (README task 5.4) — destroys the DEK(s), hard-deletes derived artifacts,
// and appends an EventErasure per affected session.
func runErase(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("erase", flag.ExitOnError)
	tenantName := fs.String("tenant", "", "tenant name to erase (mutually exclusive with --session)")
	sessionArg := fs.String("session", "", "session id to erase (mutually exclusive with --tenant)")
	reason := fs.String("reason", "operator-requested erasure", "reason recorded on the EventErasure record")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*tenantName == "") == (*sessionArg == "") {
		return fmt.Errorf("exactly one of --tenant or --session is required")
	}

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
	_ = crypto.NewKeyStore(kek) // erasure only shreds/reads encryption_keys rows directly; KeyStore isn't needed for that path itself

	signer := audit.NewSignerClient(envOr("NEXUS_SIGNERD_SOCKET", defaultSignerdSocket))
	chain := audit.NewChain(signer)

	if *tenantName != "" {
		tenantID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("nexus-agent-demo/tenant/"+*tenantName))
		var result crypto.ErasureResult
		err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var eerr error
			result, eerr = crypto.EraseTenant(ctx, tx, chain, tenantID, *reason)
			return eerr
		})
		if err != nil {
			return fmt.Errorf("erase tenant %s: %w", *tenantName, err)
		}
		reclaimArtifacts(result)
		fmt.Printf("erased tenant %q: %d key(s) shredded, %d session(s), %d derived artifact(s) removed\n",
			*tenantName, len(result.ShreddedKeyIDs), len(result.ErasureEvents), len(result.DeletedArtifacts))
		return nil
	}

	sessionID, err := uuid.Parse(*sessionArg)
	if err != nil {
		return fmt.Errorf("invalid --session: %w", err)
	}
	// A session's tenant isn't known up front from the id alone — and pool
	// connects as nexus_app, RLS-restricted to whatever app.tenant_id the
	// CURRENT transaction set, which is nothing yet (same reason
	// listTenantIDs above connects as the admin superuser instead of
	// through st): resolving "which tenant owns this session" is
	// necessarily a cross-tenant admin lookup, done here the same way.
	adminDSN := envOr("NEXUS_ADMIN_DATABASE_URL", envOr("NEXUS_MIGRATE_DATABASE_URL", defaultMigrateDSN))
	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("connect as admin: %w", err)
	}
	defer adminPool.Close()
	var tenantID uuid.UUID
	if err := adminPool.QueryRow(ctx, `SELECT tenant_id FROM sessions WHERE session_id = $1`, sessionID).Scan(&tenantID); err != nil {
		return fmt.Errorf("resolve tenant for session %s: %w", sessionID, err)
	}
	var result crypto.ErasureResult
	err = st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var eerr error
		result, eerr = crypto.EraseSession(ctx, tx, chain, tenantID, sessionID, *reason)
		return eerr
	})
	if err != nil {
		return fmt.Errorf("erase session %s: %w", sessionID, err)
	}
	reclaimArtifacts(result)
	fmt.Printf("erased session %s: %d key(s) shredded, %d derived artifact(s) removed\n", sessionID, len(result.ShreddedKeyIDs), len(result.DeletedArtifacts))
	return nil
}

// reclaimArtifacts best-effort unlinks the files EraseTenant/EraseSession
// already hard-deleted the derived_artifacts ROWS for — file removal can't
// be transactional with Postgres, so a failure here is logged, not fatal:
// crypto.ReconcileDerivedArtifacts is the backstop for exactly this case.
func reclaimArtifacts(result crypto.ErasureResult) {
	for _, a := range result.DeletedArtifacts {
		if err := os.Remove(a.Path); err != nil && !os.IsNotExist(err) {
			slog.Warn("nexusd: failed to unlink derived artifact after erasure", "path", a.Path, "error", err)
		}
	}
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
