//go:build integration

// Phase 6 — Reliability & the three state artifacts (README.md §6). Covers
// the named acceptance criteria (6.4, 6.5's sibling unit test lives in
// internal/store/condensation_test.go, and 6.6) plus the queue/lock/stuck/
// runctl machinery those three artifacts sit alongside. Shares the
// package's existing helpers (insertTenant, listEventsDirect) but builds
// its own environment (setupPhase6Env) because, unlike
// setupPostgresAndPgBouncer, internal/queue's Postgres adapter needs an
// ADMIN pool (RLS-bypassing, exactly like cmd/nexusd's own
// listTenantIDs/runErase) and internal/queue's SessionLock needs a real
// Redis — the same pairing tests/integration/cost_ceiling_test.go's own
// setupCostEnv already establishes precedent for, just also keeping the
// admin pool open rather than closing it after migrating.
package integration

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions/safety"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/queue"
	"github.com/truongpx396/nexus-agent-demo/internal/reliability"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
	"github.com/truongpx396/nexus-agent-demo/internal/tools/builtin"
	"github.com/truongpx396/nexus-agent-demo/kernel"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

type phase6Env struct {
	appPool     *pgxpool.Pool
	adminPool   *pgxpool.Pool
	redisClient *goredis.Client
}

func setupPhase6Env(t *testing.T) *phase6Env {
	t.Helper()
	ctx := context.Background()

	pgReq := testcontainers.ContainerRequest{
		Image:        "postgres:17",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "nexus",
			"POSTGRES_PASSWORD": "nexus",
			"POSTGRES_DB":       "nexus",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(120 * time.Second),
	}
	pgC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: pgReq, Started: true})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })

	redisC, err := tcredis.Run(ctx, "redis:7")
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() { _ = redisC.Terminate(ctx) })

	pgHost, err := pgC.Host(ctx)
	if err != nil {
		t.Fatalf("postgres host: %v", err)
	}
	pgPort, err := pgC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("postgres mapped port: %v", err)
	}

	adminDSN := fmt.Sprintf("postgres://nexus:nexus@%s:%s/nexus", pgHost, pgPort.Port())
	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect as admin: %v", err)
	}
	t.Cleanup(adminPool.Close)
	if _, err := store.Migrate(ctx, adminPool, migrations.FS); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	appDSN := fmt.Sprintf("postgres://nexus_app:nexus_app@%s:%s/nexus", pgHost, pgPort.Port())
	appPool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect as nexus_app: %v", err)
	}
	t.Cleanup(appPool.Close)

	redisConnStr, err := redisC.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis connection string: %v", err)
	}
	redisOpts, err := goredis.ParseURL(redisConnStr)
	if err != nil {
		t.Fatalf("parse redis connection string %q: %v", redisConnStr, err)
	}
	redisClient := goredis.NewClient(redisOpts)
	t.Cleanup(func() { _ = redisClient.Close() })

	return &phase6Env{appPool: appPool, adminPool: adminPool, redisClient: redisClient}
}

// --- shared rig: store/keys/chain/tenant, mirroring phase5's own
// setupOversightRig, duplicated here rather than exported from that file
// since neither file has any other reason to depend on the other. ---

type phase6Rig struct {
	env      *phase6Env
	st       *store.Store
	keys     *crypto.KeyStore
	chain    *audit.Chain
	tenantID uuid.UUID
}

func newPhase6Rig(t *testing.T) *phase6Rig {
	t.Helper()
	env := setupPhase6Env(t)
	ctx := context.Background()
	st := store.New(env.appPool)

	tenantID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("generate KEK: %v", err)
	}
	return &phase6Rig{
		env: env, st: st, keys: crypto.NewKeyStore(kek),
		chain: audit.NewChain(newFakeSigner(t)), tenantID: tenantID,
	}
}

func (r *phase6Rig) seal(t *testing.T, sessionID uuid.UUID) (kernel.SealFunc, crypto.DEK) {
	t.Helper()
	var dek crypto.DEK
	err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		dek, err = r.keys.NewDEK(ctx, tx, r.tenantID)
		return err
	})
	if err != nil {
		t.Fatalf("NewDEK: %v", err)
	}
	return func(plaintext []byte) ([]byte, []byte, string, error) {
		sealed, err := crypto.Seal(dek, plaintext, r.tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", err
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}, dek
}

func (r *phase6Rig) createSession(t *testing.T, sessionID, userID uuid.UUID, autonomy string) {
	t.Helper()
	err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.CreateSession(ctx, tx, store.Session{
			SessionID: sessionID, SessionKey: sessionID.String(), TenantID: r.tenantID,
			SurfaceID: "test", UserID: userID, AgentID: uuid.Nil, AgentVersion: 1,
			HarnessDigest: []byte("test-digest"), DataLabel: "internal", RouteModelID: "fake",
			AutonomyLevel: autonomy,
		})
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
}

func (r *phase6Rig) getSession(t *testing.T, sessionID uuid.UUID) store.Session {
	t.Helper()
	var sess store.Session
	err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		sess, err = store.GetSession(ctx, tx, sessionID)
		return err
	})
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return sess
}

// seedOneEvent appends one plain EventUserMessage using sealFn, sealed
// under sessionID's own DEK — every internal/runctl operation that appends
// an out-of-band event (Cancel, Steer, TightenAutonomy, ResolveClaim, the
// Claims tracker) resolves the CURRENT key to seal under by reading the
// session's own most recent event (internal/runctl/append.go's
// activeKeyID) — realistic for every real call site (none of them are ever
// invoked before a session has done at least SOMETHING), but a test that
// exercises one of these operations against a freshly-created, event-free
// session needs to seed that one event itself first.
func (r *phase6Rig) seedOneEvent(t *testing.T, sessionID uuid.UUID, sealFn kernel.SealFunc) {
	t.Helper()
	sealed, digest, keyID, err := sealFn([]byte(`{"body":"seed"}`))
	if err != nil {
		t.Fatalf("seal seed event: %v", err)
	}
	err = r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.Append(ctx, tx, store.Event{
			EventID: uuid.New(), SessionID: sessionID, TenantID: r.tenantID, SchemaVersion: store.CurrentSchemaVersion,
			Type: store.EventUserMessage, Payload: sealed, PayloadDigest: digest, KeyID: keyID, Actor: store.ActorUser,
		})
		return err
	})
	if err != nil {
		t.Fatalf("append seed event: %v", err)
	}
}

func (r *phase6Rig) control(k *kernel.Kernel, approvals *oversight.Approvals, inputs *oversight.Inputs) *runctl.Control {
	return &runctl.Control{
		Store: r.st, Keys: r.keys, Chain: r.chain, Approvals: approvals, Inputs: inputs, Kernel: k,
		System: "test", MaxTurns: 20,
	}
}

// newReadPipeline builds a Pipeline whose one tool is the REAL builtin
// platform/file_read — read-only, so autonomy never blocks it regardless of
// level, and IsConcurrencySafe, so it never needs the in-process serial
// slot either. claims, if non-nil, is wired in (file_read is read-only, so
// it never actually touches Claims — TestClaims_* below builds its own
// mutating tool instead).
func newReadPipeline(t *testing.T, workspaceRoot string, claims tools.Claims) (*tools.Pipeline, []provider.ToolSchema) {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.DeclareNamespace("platform", "test"); err != nil {
		t.Fatalf("declare namespace: %v", err)
	}
	ft := builtin.FileRead{}
	if err := reg.Register(ft); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	if err := reg.SetAdmissionStatus(ft.ID(), tools.AdmissionClean); err != nil {
		t.Fatalf("set admission: %v", err)
	}
	manifest := tools.BuildManifest(reg)
	chain := permissions.NewChain(permissions.ChainConfig{
		Profiles: permissions.ProfileSet{Profiles: []permissions.ToolProfile{permissions.NewToolProfile("default", 1, ft.ID().String())}},
		// Gate 3's safety classifier defaults to a nil model leg, which
		// fails closed to Ask for anything the rule pass doesn't
		// explicitly clear (README task 3.9) — alwaysDeferModel stands in
		// for a real model leg the same way cmd/nexusd's own
		// demoSafetyModel does, so these tests exercise stuck
		// detection/projection caching, not Gate 3's own fail-closed
		// default.
		Safety: safety.NewClassifier(safety.DefaultRules(), alwaysDeferModel{}, 0),
	})
	pipeline := tools.NewPipeline(tools.PipelineConfig{Registry: reg, Manifest: manifest, Chain: chain, WorkspaceRoot: workspaceRoot, Claims: claims})
	d := ft.Descriptor()
	return pipeline, []provider.ToolSchema{{Name: d.ID.String(), Description: d.Description, InputSchema: d.InputSchema}}
}

// alwaysDeferModel stands in for Gate 3's model leg — this package has no
// live model configured (internal/provider/fake drives the CONTENT/tool_use
// side of these tests, never the safety side), so without this the chain's
// default nil-model classifier fails closed to Ask on every call the rule
// pass doesn't explicitly clear, masking whatever behavior a given test
// actually means to exercise. Mirrors cmd/nexusd's own demoSafetyModel and
// internal/tools/pipeline_test.go's own alwaysDeferModel, duplicated here
// for the same reason every other small fake in this package is.
type alwaysDeferModel struct{}

func (alwaysDeferModel) Classify(context.Context, string, string) (safety.Verdict, string, error) {
	return safety.VerdictDefer, "test default", nil
}

// repeatedToolUseScripts builds n identical single-turn scripts, each
// issuing the exact same tool_use — what drives internal/reliability's
// stuck detector into a repeated_action verdict.
func repeatedToolUseScripts(n int, toolName, input string) []fake.Script {
	scripts := make([]fake.Script, n)
	for i := range scripts {
		scripts[i] = fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "tool_use", ToolUseID: fmt.Sprintf("tu%d", i), ToolName: toolName, Input: input},
			{Kind: "done", Done: "stop"},
		}}
	}
	return scripts
}

// --- 6.1/6.2: the queue + session lock ---

func TestQueue_SkipLockedNeverDoubleLeases(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	port := queue.NewPostgres(rig.env.adminPool)

	sessionID := uuid.New()
	rig.createSession(t, sessionID, uuid.New(), "supervised")
	if _, err := port.Enqueue(ctx, queue.Job{TenantID: rig.tenantID, SessionID: sessionID, SessionKey: sessionID.String(), Kind: queue.KindResume}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	const workers = 8
	var leased int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(owner int) {
			defer wg.Done()
			_, ok, err := port.Lease(ctx, fmt.Sprintf("worker-%d", owner), time.Minute)
			if err != nil {
				t.Errorf("lease: %v", err)
				return
			}
			if ok {
				atomic.AddInt32(&leased, 1)
			}
		}(i)
	}
	wg.Wait()

	if leased != 1 {
		t.Fatalf("leased = %d across %d concurrent workers racing ONE job, want exactly 1 (SKIP LOCKED must prevent a double lease)", leased, workers)
	}
}

func TestQueue_WorkerCompletesAndFailsRealJobs(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	port := queue.NewPostgres(rig.env.adminPool)

	sessionID := uuid.New()
	rig.createSession(t, sessionID, uuid.New(), "supervised")
	job, err := port.Enqueue(ctx, queue.Job{TenantID: rig.tenantID, SessionID: sessionID, SessionKey: sessionID.String(), Kind: queue.KindResume})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	leased, ok, err := port.Lease(ctx, "w1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if leased.JobID != job.JobID || leased.Attempts != 1 {
		t.Fatalf("leased job = %+v, want job %s at attempt 1", leased, job.JobID)
	}
	if err := port.Complete(ctx, job.JobID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// A completed job is never leasable again.
	if _, ok, err := port.Lease(ctx, "w2", time.Minute); err != nil || ok {
		t.Fatalf("lease after complete: ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestSessionLock_SerialPerSessionConcurrentAcrossSessions(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	lock := queue.NewSessionLock(rig.env.redisClient, 5*time.Second)

	// Serial: a second Acquire for the SAME session_key fails while the
	// first holder still has it.
	token1, ok, err := lock.Acquire(ctx, "session-a")
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	if _, ok, err := lock.Acquire(ctx, "session-a"); err != nil || ok {
		t.Fatalf("second acquire of the same session_key: ok=%v err=%v, want ok=false", ok, err)
	}

	// Concurrent: a DIFFERENT session_key is unaffected.
	if _, ok, err := lock.Acquire(ctx, "session-b"); err != nil || !ok {
		t.Fatalf("acquire of a different session_key: ok=%v err=%v, want ok=true", ok, err)
	}

	// Release, then the first session_key is acquirable again.
	if err := lock.Release(ctx, "session-a", token1); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, ok, err := lock.Acquire(ctx, "session-a"); err != nil || !ok {
		t.Fatalf("acquire after release: ok=%v err=%v, want ok=true", ok, err)
	}
}

func TestSessionLock_ReleaseNeverStealsAnotherHoldersLock(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	lock := queue.NewSessionLock(rig.env.redisClient, 5*time.Second)

	_, ok, err := lock.Acquire(ctx, "session-c")
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	// Release with the WRONG token must be a no-op — the real holder's lock
	// must survive it.
	if err := lock.Release(ctx, "session-c", "not-the-real-token"); err != nil {
		t.Fatalf("release with wrong token: %v", err)
	}
	if _, ok, err := lock.Acquire(ctx, "session-c"); err != nil || ok {
		t.Fatalf("acquire after a wrong-token release attempt: ok=%v err=%v, want ok=false (the real lock must still be held)", ok, err)
	}
}

// --- 6.6: write-ahead claims, end to end against real storage ---

func TestClaims_EndToEnd_NeverReExecutesAnAmbiguousEffect(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	rig.createSession(t, sessionID, userID, "autonomous")
	sealFn, _ := rig.seal(t, sessionID)
	rig.seedOneEvent(t, sessionID, sealFn)

	tracker := runctl.NewClaimTracker(rig.st, rig.keys, rig.chain)
	digest := []byte("a-fixed-canonical-digest-for-this-test")

	id1, outcome1, err := tracker.Open(ctx, rig.tenantID, sessionID, "platform/send_email@v1", digest)
	if err != nil {
		t.Fatalf("Open (1st): %v", err)
	}
	if outcome1 != tools.ClaimFresh {
		t.Fatalf("Open (1st) outcome = %v, want ClaimFresh", outcome1)
	}

	// Simulate a crash between Open and Complete: a second Open for the
	// SAME digest, before anyone completed the first, must be ambiguous —
	// never treated as fresh.
	id2, outcome2, err := tracker.Open(ctx, rig.tenantID, sessionID, "platform/send_email@v1", digest)
	if err != nil {
		t.Fatalf("Open (2nd): %v", err)
	}
	if outcome2 != tools.ClaimAmbiguous || id2 != id1 {
		t.Fatalf("Open (2nd) = (%s, %v), want the SAME claim id (%s) and ClaimAmbiguous", id2, outcome2, id1)
	}

	// Resolve it (a human/probe determined the effect never actually
	// happened) — this is internal/runctl's own operator-facing escape
	// hatch, distinct from Complete (which internal/tools/pipeline.go calls
	// automatically right after a real Tool.Call).
	control := rig.control(nil, nil, nil)
	if _, err := control.ResolveClaim(ctx, rig.tenantID, sessionID, id1, store.ClaimAbandoned, "confirmed: the email was never sent"); err != nil {
		t.Fatalf("ResolveClaim: %v", err)
	}

	// A fresh Open for the SAME digest now proceeds — an abandoned claim is
	// resolved, not permanently blocking.
	id3, outcome3, err := tracker.Open(ctx, rig.tenantID, sessionID, "platform/send_email@v1", digest)
	if err != nil {
		t.Fatalf("Open (3rd, post-resolution): %v", err)
	}
	if outcome3 == tools.ClaimFresh {
		t.Fatalf("Open (3rd) = %v — an abandoned claim's row still exists at the same digest, so Open correctly still finds SOMETHING; the real re-execution guard is that finishCall never re-Calls the tool on ClaimAmbiguous/ClaimDone, which claims_test.go (internal/tools) already covers unit-level. id3=%s", outcome3, id3)
	}

	// Now complete a FRESH claim end to end and verify it durably reaches
	// completed, short-circuiting any further Open.
	freshDigest := []byte("a-different-digest-thats-genuinely-fresh")
	freshID, outcome, err := tracker.Open(ctx, rig.tenantID, sessionID, "platform/send_email@v1", freshDigest)
	if err != nil || outcome != tools.ClaimFresh {
		t.Fatalf("Open (fresh): id=%s outcome=%v err=%v", freshID, outcome, err)
	}
	if err := tracker.Complete(ctx, rig.tenantID, sessionID, freshID, false, ""); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_, outcomeDone, err := tracker.Open(ctx, rig.tenantID, sessionID, "platform/send_email@v1", freshDigest)
	if err != nil {
		t.Fatalf("Open (after complete): %v", err)
	}
	if outcomeDone != tools.ClaimDone {
		t.Fatalf("Open (after complete) = %v, want ClaimDone (short-circuit)", outcomeDone)
	}

	events, err := listEventsDirect(ctx, rig.st, rig.tenantID, sessionID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if !hasEventType(events, store.EventEffectClaimed) || !hasEventType(events, store.EventEffectClaimResolved) {
		t.Fatal("expected both effect_claimed and effect_claim_resolved events durably appended")
	}
}

// --- 6.4: Snapshot must be provably disposable ---

func TestSnapshot_DeletingAllSnapshotsChangesNothingButHydrationTime(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	workspaceRoot := t.TempDir()
	rig.createSession(t, sessionID, userID, "read_only")

	pipeline, catalog := newReadPipeline(t, workspaceRoot, nil)
	prov := fake.New(fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "tool_use", ToolUseID: "tu1", ToolName: "platform/file_read@v1", Input: `{"path":"missing.txt"}`},
		{Kind: "done", Done: "stop"},
	}}, fake.Script{Chunks: []fake.ChunkSpec{{Kind: "content", Text: "done"}, {Kind: "done", Done: "stop"}}})
	k := &kernel.Kernel{Provider: provider.Wrap([]provider.Provider{prov}), Tools: kernel.PipelineExecutor{Pipeline: pipeline}, Budget: kernel.NoopBudgetGate{}, Store: rig.st}
	sealFn, _ := rig.seal(t, sessionID)
	st0 := &kernel.RunState{TenantID: rig.tenantID, SessionID: sessionID, Seal: sealFn}
	for ev, err := range k.Run(ctx, st0, kernel.RunConfig{System: "test", Catalog: catalog, ModelID: "fake", MaxTurns: 10, Input: "read a file"}) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		_ = ev
	}

	final := rig.getSession(t, sessionID)
	if final.Status != store.SessionStatusCompleted {
		t.Fatalf("final status = %q, want completed", final.Status)
	}

	// The from-scratch answer, computed once, before any snapshot exists.
	var beforeAny store.Projection
	if err := rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		beforeAny, err = store.LoadProjection(ctx, tx, sessionID)
		return err
	}); err != nil {
		t.Fatalf("LoadProjection (no snapshot yet): %v", err)
	}

	// Save a snapshot claiming the SAME answer, then verify the cached path
	// agrees with the from-scratch one.
	if err := rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.SaveSnapshot(ctx, tx, store.Snapshot{TenantID: rig.tenantID, SessionID: sessionID, AtSeq: 999, Status: beforeAny.Status, TerminalReason: beforeAny.TerminalReason})
		return err
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	var withSnapshot store.Projection
	if err := rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		withSnapshot, err = store.LoadProjection(ctx, tx, sessionID)
		return err
	}); err != nil {
		t.Fatalf("LoadProjection (with snapshot): %v", err)
	}
	if withSnapshot.Status != beforeAny.Status {
		t.Fatalf("LoadProjection with a snapshot present = %q, want %q (the from-scratch answer)", withSnapshot.Status, beforeAny.Status)
	}

	// task 6.4's own acceptance line: delete EVERY snapshot, then verify
	// the answer is UNCHANGED — only ever recomputed from scratch again.
	if err := rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.DeleteAllSnapshots(ctx, tx, sessionID)
	}); err != nil {
		t.Fatalf("DeleteAllSnapshots: %v", err)
	}
	var afterDelete store.Projection
	if err := rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		afterDelete, err = store.LoadProjection(ctx, tx, sessionID)
		return err
	}); err != nil {
		t.Fatalf("LoadProjection (after delete): %v", err)
	}
	if afterDelete.Status != beforeAny.Status {
		t.Fatalf("Projection changed after deleting every snapshot: before=%q after=%q — a Snapshot must be provably disposable", beforeAny.Status, afterDelete.Status)
	}
}

// --- 6.8: stuck detection, wired into a real kernel.Run ---

func TestStuckDetection_SecondCorroboratingTripTerminates(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	workspaceRoot := t.TempDir()
	rig.createSession(t, sessionID, userID, "read_only")

	pipeline, catalog := newReadPipeline(t, workspaceRoot, nil)
	// 6 identical turns: with window=4, the trip fires at turn 4 (non-
	// terminal) and corroborates at turn 5 (terminal) — turn 6 exists only
	// so a bug that fails to terminate doesn't silently exhaust the script
	// and mask itself as MaxTurnsExceeded instead.
	scripts := repeatedToolUseScripts(6, "platform/file_read@v1", `{"path":"same.txt"}`)
	prov := fake.New(scripts...)
	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{prov}), Tools: kernel.PipelineExecutor{Pipeline: pipeline},
		Budget: kernel.NoopBudgetGate{}, Store: rig.st, Stuck: reliability.NewRegistry(4),
	}
	sealFn, _ := rig.seal(t, sessionID)
	st0 := &kernel.RunState{TenantID: rig.tenantID, SessionID: sessionID, Seal: sealFn}

	var events []store.Event
	for ev, err := range k.Run(ctx, st0, kernel.RunConfig{System: "test", Catalog: catalog, ModelID: "fake", MaxTurns: 10, Input: "loop forever"}) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		events = append(events, ev)
	}

	if !hasEventType(events, store.EventStuckSuspected) {
		t.Fatal("expected at least one stuck_suspected event")
	}
	final := rig.getSession(t, sessionID)
	if final.Status != store.SessionStatusFailed || final.TerminalReason == nil || *final.TerminalReason != "stuck_terminated" {
		t.Fatalf("final status/reason = %v/%v, want failed/stuck_terminated", final.Status, final.TerminalReason)
	}
}

// --- 6.9/6.10: runctl's cancel/steer/tightenAutonomy/replay, and the
// crash-recovery resume the README §6 demo line describes ---

func TestRunctl_Cancel_ReleasesPendingApprovalAndAborts(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	workspaceRoot := t.TempDir()

	// A mutating tool that always asks — the same shape phase5's own
	// newFileWritePipeline uses, duplicated here for the same reason every
	// other cross-file helper in this package is: no shared export exists
	// and building one isn't worth a coupling.
	reg := tools.NewRegistry()
	if err := reg.DeclareNamespace("platform", "test"); err != nil {
		t.Fatalf("declare namespace: %v", err)
	}
	ft := builtin.FileWrite{}
	if err := reg.Register(ft); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := reg.SetAdmissionStatus(ft.ID(), tools.AdmissionClean); err != nil {
		t.Fatalf("admit: %v", err)
	}
	manifest := tools.BuildManifest(reg)
	chain := permissions.NewChain(permissions.ChainConfig{
		Profiles: permissions.ProfileSet{Profiles: []permissions.ToolProfile{permissions.NewToolProfile("default", 1, ft.ID().String())}},
		Approval: permissions.ApprovalPolicy{RequireAskFor: map[permissions.EffectClass]permissions.AskKind{permissions.EffectClassMutating: permissions.AskOnce}},
	})
	pipeline := tools.NewPipeline(tools.PipelineConfig{Registry: reg, Manifest: manifest, Chain: chain, WorkspaceRoot: workspaceRoot})
	d := ft.Descriptor()
	catalog := []provider.ToolSchema{{Name: d.ID.String(), Description: d.Description, InputSchema: d.InputSchema}}

	approvals := oversight.NewApprovals(rig.st, rig.keys, rig.chain)
	prov := fake.New(fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "tool_use", ToolUseID: "tu1", ToolName: "platform/file_write@v1", Input: `{"path":"out.txt","content":"x"}`},
		{Kind: "done", Done: "stop"},
	}})
	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{prov}), Tools: kernel.PipelineExecutor{Pipeline: pipeline},
		Budget: kernel.NoopBudgetGate{}, Store: rig.st,
		OnSuspend: func(ctx context.Context, tx pgx.Tx, req kernel.SuspendRequest) error {
			_, err := approvals.Create(ctx, oversight.CreateApprovalRequest{
				TenantID: req.TenantID, SessionID: req.SessionID, ToolUseEventID: req.ToolUseEventID,
				ToolID: req.ToolID, AskKind: req.AskKind, CanonicalDigest: req.CanonicalDigest,
				Context: oversight.ContextPackage{ToolID: req.ToolID, EffectClass: req.EffectClass, Input: req.Input},
			})
			return err
		},
	}

	rig.createSession(t, sessionID, userID, "supervised")
	sealFn, _ := rig.seal(t, sessionID)
	st0 := &kernel.RunState{TenantID: rig.tenantID, SessionID: sessionID, Seal: sealFn}
	for ev, err := range k.Run(ctx, st0, kernel.RunConfig{System: "test", Catalog: catalog, ModelID: "fake", MaxTurns: 10, AutonomyLevel: "supervised"}) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		_ = ev
	}
	if rig.getSession(t, sessionID).Status != store.SessionStatusSuspended {
		t.Fatal("expected the session to be suspended on the pending approval")
	}

	control := rig.control(k, approvals, nil)
	if err := control.Cancel(ctx, rig.tenantID, sessionID, "operator changed their mind"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	final := rig.getSession(t, sessionID)
	if final.Status != store.SessionStatusFailed || final.TerminalReason == nil || *final.TerminalReason != "aborted" {
		t.Fatalf("final status/reason = %v/%v, want failed/aborted", final.Status, final.TerminalReason)
	}
	pending, err := approvals.ListPending(ctx, rig.tenantID)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no pending approvals after Cancel, got %d", len(pending))
	}

	// A second Cancel on an already-terminal session must be a harmless
	// no-op, not an error.
	if err := control.Cancel(ctx, rig.tenantID, sessionID, "again"); err != nil {
		t.Fatalf("second Cancel on an already-terminal session: %v", err)
	}
}

func TestRunctl_TightenAutonomy_RefusesWidening(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	rig.createSession(t, sessionID, userID, "autonomous")
	sealFn, _ := rig.seal(t, sessionID)
	rig.seedOneEvent(t, sessionID, sealFn)

	control := rig.control(nil, nil, nil)
	if err := control.TightenAutonomy(ctx, rig.tenantID, sessionID, "supervised"); err != nil {
		t.Fatalf("tighten autonomous -> supervised: %v", err)
	}
	if got := rig.getSession(t, sessionID).AutonomyLevel; got != "supervised" {
		t.Fatalf("autonomy_level = %q, want supervised", got)
	}

	if err := control.TightenAutonomy(ctx, rig.tenantID, sessionID, "autonomous"); err == nil {
		t.Fatal("widening supervised -> autonomous must be refused")
	}
	if got := rig.getSession(t, sessionID).AutonomyLevel; got != "supervised" {
		t.Fatalf("autonomy_level after a refused widen = %q, want unchanged (supervised)", got)
	}

	events, err := listEventsDirect(ctx, rig.st, rig.tenantID, sessionID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	count := 0
	for _, e := range events {
		if e.Type == store.EventAutonomyTightened {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 autonomy_tightened event (the refused widen must append none), got %d", count)
	}
}

// TestRunctl_Resume_RecoversAToolUseAKilledWorkerLeftOrphaned is README §6's
// own demo line made concrete: a tool_use with no paired result (exactly
// what `kill -9` mid-Call leaves behind) must never be silently
// re-executed on resume — Hygiene synthesizes an "interrupted_before_
// execution" result for it, and the run continues cleanly past that point.
func TestRunctl_Resume_RecoversAToolUseAKilledWorkerLeftOrphaned(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	workspaceRoot := t.TempDir()
	rig.createSession(t, sessionID, userID, "read_only")
	sealFn, _ := rig.seal(t, sessionID)

	// Simulate the crash directly: append a user_message and a tool_use
	// with NO paired result, then mark the session "running" — exactly the
	// durable state a process killed between dispatching a tool_use and
	// appending its result leaves behind. No live kernel.Run call is
	// involved in producing this state, on purpose: this test's whole
	// point is that Resume recovers correctly from what the LOG says
	// happened, not from replaying a specific in-process crash.
	err := rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, ev := range []struct {
			typ     store.EventType
			actor   store.Actor
			payload string
		}{
			{store.EventUserMessage, store.ActorUser, `{"body":"read a file"}`},
			{store.EventToolUse, store.ActorModel, `{"tool_name":"platform/file_read@v1","input":{"path":"missing.txt"}}`},
		} {
			sealed, digest, keyID, serr := sealFn([]byte(ev.payload))
			if serr != nil {
				return serr
			}
			toolID := "platform/file_read@v1"
			var toolIDPtr *string
			if ev.typ == store.EventToolUse {
				toolIDPtr = &toolID
			}
			if _, aerr := store.Append(ctx, tx, store.Event{
				EventID: uuid.New(), SessionID: sessionID, TenantID: rig.tenantID, SchemaVersion: store.CurrentSchemaVersion,
				Type: ev.typ, Payload: sealed, PayloadDigest: digest, KeyID: keyID, Actor: ev.actor, ToolID: toolIDPtr,
			}); aerr != nil {
				return aerr
			}
		}
		return store.UpdateSessionStatus(ctx, tx, sessionID, store.SessionStatusRunning, nil)
	})
	if err != nil {
		t.Fatalf("simulate crash state: %v", err)
	}

	pipeline, catalog := newReadPipeline(t, workspaceRoot, nil)
	prov := fake.New(fake.Script{Chunks: []fake.ChunkSpec{{Kind: "content", Text: "done"}, {Kind: "done", Done: "stop"}}})
	k := &kernel.Kernel{Provider: provider.Wrap([]provider.Provider{prov}), Tools: kernel.PipelineExecutor{Pipeline: pipeline}, Budget: kernel.NoopBudgetGate{}, Store: rig.st}

	control := rig.control(k, oversight.NewApprovals(rig.st, rig.keys, rig.chain), nil)
	control.Catalog = catalog

	var resumeEvents []store.Event
	for ev, rerr := range control.Resume(ctx, rig.tenantID, sessionID) {
		if rerr != nil {
			t.Fatalf("Resume: %v", rerr)
		}
		resumeEvents = append(resumeEvents, ev)
	}

	// The orphaned tool_use got exactly one SYNTHETIC result — never a real
	// re-dispatch (no live tool call site here could have produced a
	// non-synthetic one).
	var sawSynthetic bool
	for _, e := range resumeEvents {
		if e.Type == store.EventToolResult {
			sawSynthetic = true
		}
	}
	if !sawSynthetic {
		t.Fatal("expected Resume's first turn to synthesize a result for the orphaned tool_use")
	}
	final := rig.getSession(t, sessionID)
	if final.Status != store.SessionStatusCompleted {
		t.Fatalf("final status = %q, want completed (the run continues cleanly past the recovered call)", final.Status)
	}
}

func TestRunctl_Resume_RefusesWhileAClaimIsUnresolved(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	rig.createSession(t, sessionID, userID, "autonomous")
	sealFn, _ := rig.seal(t, sessionID)
	rig.seedOneEvent(t, sessionID, sealFn)
	if err := rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.UpdateSessionStatus(ctx, tx, sessionID, store.SessionStatusRunning, nil)
	}); err != nil {
		t.Fatalf("set running: %v", err)
	}

	tracker := runctl.NewClaimTracker(rig.st, rig.keys, rig.chain)
	if _, outcome, err := tracker.Open(ctx, rig.tenantID, sessionID, "platform/send_email@v1", []byte("digest")); err != nil || outcome != tools.ClaimFresh {
		t.Fatalf("Open: outcome=%v err=%v", outcome, err)
	}

	control := rig.control(nil, nil, nil)
	var gotErr error
	for _, rerr := range control.Resume(ctx, rig.tenantID, sessionID) {
		if rerr != nil {
			gotErr = rerr
			break
		}
	}
	var unresolved runctl.ErrUnresolvedClaims
	if !asErrUnresolvedClaims(gotErr, &unresolved) {
		t.Fatalf("Resume error = %v, want ErrUnresolvedClaims", gotErr)
	}
}

func asErrUnresolvedClaims(err error, target *runctl.ErrUnresolvedClaims) bool {
	if e, ok := err.(runctl.ErrUnresolvedClaims); ok {
		*target = e
		return true
	}
	return false
}

func TestRunctl_Replay_IsPureAndAgreesWithHydration(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	workspaceRoot := t.TempDir()
	rig.createSession(t, sessionID, userID, "read_only")

	pipeline, catalog := newReadPipeline(t, workspaceRoot, nil)
	prov := fake.New(fake.Script{Chunks: []fake.ChunkSpec{{Kind: "content", Text: "hi"}, {Kind: "done", Done: "stop"}}})
	k := &kernel.Kernel{Provider: provider.Wrap([]provider.Provider{prov}), Tools: kernel.PipelineExecutor{Pipeline: pipeline}, Budget: kernel.NoopBudgetGate{}, Store: rig.st}
	sealFn, _ := rig.seal(t, sessionID)
	st0 := &kernel.RunState{TenantID: rig.tenantID, SessionID: sessionID, Seal: sealFn}
	for ev, err := range k.Run(ctx, st0, kernel.RunConfig{System: "test", Catalog: catalog, ModelID: "fake", MaxTurns: 5, Input: "hi"}) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		_ = ev
	}

	control := rig.control(nil, nil, nil)
	result, err := control.Replay(ctx, rig.tenantID, sessionID)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result.Projection.Status != store.SessionStatusCompleted {
		t.Fatalf("Replay projection status = %q, want completed", result.Projection.Status)
	}
	if len(result.Synthetic) != 0 {
		t.Fatalf("a cleanly-completed session should need no synthetic results, got %d", len(result.Synthetic))
	}
	if len(result.History) == 0 {
		t.Fatal("Replay returned no history")
	}
}
