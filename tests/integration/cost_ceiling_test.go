//go:build integration

// The Phase 4 acceptance criterion (README.md §5): fire concurrent sessions
// against ONE TENANT ceiling and prove total spend never exceeds it — "this
// is what post-hoc aggregation cannot deliver." A naive check-then-increment
// would let concurrent goroutines all observe room and all proceed,
// overshooting the ceiling; internal/cost/redis.go's Lua script makes the
// check-and-reserve one atomic round trip instead, so this test is the one
// place that property is actually exercised under real concurrency against
// a real Redis — internal/cost's own unit tests (money/meter/pricebook) are
// deliberately I/O-free and can't prove this.
package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// setupCostEnv starts a single postgres container (no PgBouncer needed —
// this test is about Redis-atomic ceiling enforcement, not connection
// pooling; see internal/crypto/keystore_integration_test.go for the same
// call on the same tradeoff) and a real Redis container, applies
// migrations, and returns a pool connected as nexus_app plus a ready
// *redis.Client.
func setupCostEnv(t *testing.T) (*pgxpool.Pool, *goredis.Client) {
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

	migrateDSN := fmt.Sprintf("postgres://nexus:nexus@%s:%s/nexus", pgHost, pgPort.Port())
	migratePool, err := pgxpool.New(ctx, migrateDSN)
	if err != nil {
		t.Fatalf("connect as migration role: %v", err)
	}
	defer migratePool.Close()
	if _, err := store.Migrate(ctx, migratePool, migrations.FS); err != nil {
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

	return appPool, redisClient
}

func identitySeal(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
	return plaintext, plaintext, "test-key", nil
}

func TestCostGate_ConcurrentSessionsRespectOneTenantCeiling(t *testing.T) {
	pool, redisClient := setupCostEnv(t)
	ctx := context.Background()
	st := store.New(pool)

	tenantID := uuid.New()
	if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'cost-ceiling-test')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	// Price book: $3/million input tokens, $15/million output tokens — the
	// same figures cmd/nexusd's seedPriceBook uses, so this test's expected
	// worst-case reservation matches what a real `nexusd` deployment would
	// compute.
	if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now()
		for _, p := range []struct {
			meter  cost.MeterID
			micros int64
		}{
			{cost.MeterInputUncached, 3_000_000},
			{cost.MeterOutput, 15_000_000},
		} {
			if err := cost.InsertPriceBookEntry(ctx, tx, tenantID, cost.PriceBookEntry{
				Meter: p.meter, Subject: cost.WildcardSubject, Version: 1,
				Currency: cost.DefaultCurrency, PricePerMillionMicros: p.micros, EffectiveFrom: now,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed price book: %v", err)
	}

	gateCfg := cost.GateConfig{} // production defaults: 8000 input / 4096 output token worst-case estimate
	gate := cost.NewGate(st, redisClient, cost.DefaultMeters(), gateCfg)

	// Worst-case reservation per call, using the SAME defaults the Gate
	// just applied: 8000 * $3/M + 4096 * $15/M = 24,000 + 61,440 =
	// 85,440 micros. A ceiling of exactly 5x that lets exactly 5 of 20
	// concurrent reservations through, deterministically — every
	// reservation here asks for the identical amount, so however the 20
	// goroutines interleave, the atomic Redis counter admits exactly
	// floor(ceiling / worstCase) = 5 and refuses the rest. That
	// determinism is what makes this a real assertion instead of a
	// probabilistic one.
	const perCallMicros = 8_000*3_000_000/1_000_000 + 4_096*15_000_000/1_000_000 // 85_440
	const wantSuccesses = 5
	ceilingMicros := int64(wantSuccesses * perCallMicros)

	var budgetID uuid.UUID
	if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := cost.CreateBudget(ctx, tx, tenantID, cost.BudgetScopeTenant, nil, cost.Money{Micros: ceilingMicros, Currency: cost.DefaultCurrency})
		budgetID = b.ID
		return err
	}); err != nil {
		t.Fatalf("create tenant budget: %v", err)
	}

	const numSessions = 20
	scripts := make([]fake.Script, numSessions)
	for i := range scripts {
		scripts[i] = fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "content", Text: "hello"},
			{Kind: "usage", InputUncached: 50, OutputTokens: 10},
			{Kind: "done", Done: "stop"},
		}}
	}
	fakeProvider := fake.New(scripts...)

	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fakeProvider}),
		Tools:    kernel.NotImplementedToolExecutor{},
		Budget:   gate,
		Store:    st,
	}

	sessionIDs := make([]uuid.UUID, numSessions)
	for i := range sessionIDs {
		sessionIDs[i] = uuid.New()
		if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return store.CreateSession(ctx, tx, store.Session{
				SessionID: sessionIDs[i], SessionKey: sessionIDs[i].String(), TenantID: tenantID,
				SurfaceID: "test", UserID: uuid.New(), AgentID: uuid.Nil, AgentVersion: 1,
				HarnessDigest: []byte("test-digest"), AutonomyLevel: "supervised",
			})
		}); err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	for _, sessionID := range sessionIDs {
		wg.Add(1)
		go func(sessionID uuid.UUID) {
			defer wg.Done()
			rst := &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: identitySeal}
			cfg := kernel.RunConfig{System: "you are a test agent", Input: "go", MaxTurns: 5}
			for _, err := range k.Run(ctx, rst, cfg) {
				if err != nil {
					t.Errorf("session %s: kernel.Run yielded an error: %v", sessionID, err)
					return
				}
			}
		}(sessionID)
	}
	wg.Wait()

	// Every Reserve resolution — allowed or refused — left a
	// budget_decisions row (README task 4.6). Count both classes and sum
	// what was actually reserved by the successful ones.
	var succeeded, refused int
	var reservedSum int64
	if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT decision, reserved_micros FROM budget_decisions WHERE budget_id = $1`, budgetID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var decision string
			var reserved int64
			if err := rows.Scan(&decision, &reserved); err != nil {
				return err
			}
			switch decision {
			case string(cost.DecisionAllow), string(cost.DecisionDegrade):
				succeeded++
				reservedSum += reserved
			case string(cost.DecisionRefuseCeiling):
				refused++
			default:
				t.Errorf("unexpected decision kind %q against the tenant budget", decision)
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("query budget_decisions: %v", err)
	}

	if succeeded != wantSuccesses {
		t.Errorf("succeeded reservations = %d, want exactly %d (ceiling=%d, per-call=%d)", succeeded, wantSuccesses, ceilingMicros, perCallMicros)
	}
	if refused != numSessions-wantSuccesses {
		t.Errorf("refused reservations = %d, want exactly %d", refused, numSessions-wantSuccesses)
	}
	if reservedSum > ceilingMicros {
		t.Fatalf("THE ACCEPTANCE CRITERION: reserved sum %d micros EXCEEDED the tenant ceiling %d micros under concurrency", reservedSum, ceilingMicros)
	}
	if reservedSum != ceilingMicros {
		// Not just <=: with every reservation identical in size, the atomic
		// counter should admit exactly enough to reach the ceiling exactly,
		// never less (that would mean a request that should have fit was
		// wrongly refused) and never more (the property under test).
		t.Errorf("reserved sum = %d micros, want exactly %d (ceiling filled exactly, not over- or under-admitted)", reservedSum, ceilingMicros)
	}

	// Terminal reasons agree with the decisions: a refused reservation ends
	// its run cost_exhausted; an admitted one completes normally.
	var completedCount, costExhaustedCount int
	for _, sessionID := range sessionIDs {
		var status string
		var reason *string
		if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			sess, err := store.GetSession(ctx, tx, sessionID)
			if err != nil {
				return err
			}
			status, reason = sess.Status, sess.TerminalReason
			return nil
		}); err != nil {
			t.Fatalf("get session %s: %v", sessionID, err)
		}
		if status == store.SessionStatusCompleted {
			completedCount++
		} else if reason != nil && *reason == "cost_exhausted" {
			costExhaustedCount++
		} else {
			t.Errorf("session %s ended status=%s reason=%v, want completed or cost_exhausted", sessionID, status, reason)
		}
	}
	if completedCount != wantSuccesses {
		t.Errorf("completed sessions = %d, want %d", completedCount, wantSuccesses)
	}
	if costExhaustedCount != numSessions-wantSuccesses {
		t.Errorf("cost_exhausted sessions = %d, want %d", costExhaustedCount, numSessions-wantSuccesses)
	}

	// And the reconciled truth (cost_records) for the successful sessions
	// stays well under what was reserved — Reconcile released the unused
	// worst-case margin back once the real (much smaller, scripted) usage
	// was known.
	var actualSpendSum int64
	if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(sum(minor_units), 0) FROM cost_records WHERE tenant_id = $1`, tenantID).Scan(&actualSpendSum)
	}); err != nil {
		t.Fatalf("sum cost_records: %v", err)
	}
	if actualSpendSum <= 0 || actualSpendSum >= ceilingMicros {
		t.Errorf("actual reconciled spend = %d micros, want >0 and well under the %d micros ceiling (reconciliation should have released the unused worst-case margin)", actualSpendSum, ceilingMicros)
	}
}

// TestCostGate_UnreportedUsageReconcilesAtFullReservedCost is README task
// 4.7: when a stream fails before ever reporting usage, Reconcile must
// charge the FULL reserved worst case rather than trusting a partial/zero
// usage figure — "an unreliable provider must not look free." The scripted
// stream here errors on its second chunk with no usage chunk ever emitted
// (provider/fake.Script's Malformed flag), and provider.Wrap never retries
// past the first chunk it has already peeked for the caller — with exactly
// one provider in the Wrap list there is nowhere to fail over to anyway, so
// this is a clean single call, not a multi-attempt race.
func TestCostGate_UnreportedUsageReconcilesAtFullReservedCost(t *testing.T) {
	pool, redisClient := setupCostEnv(t)
	ctx := context.Background()
	st := store.New(pool)

	tenantID := uuid.New()
	if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'cost-unreported-test')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now()
		for _, p := range []struct {
			meter  cost.MeterID
			micros int64
		}{
			{cost.MeterInputUncached, 3_000_000},
			{cost.MeterOutput, 15_000_000},
		} {
			if err := cost.InsertPriceBookEntry(ctx, tx, tenantID, cost.PriceBookEntry{
				Meter: p.meter, Subject: cost.WildcardSubject, Version: 1,
				Currency: cost.DefaultCurrency, PricePerMillionMicros: p.micros, EffectiveFrom: now,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed price book: %v", err)
	}

	gate := cost.NewGate(st, redisClient, cost.DefaultMeters(), cost.GateConfig{})
	const perCallMicros = 8_000*3_000_000/1_000_000 + 4_096*15_000_000/1_000_000 // 85_440, same worst-case math as the ceiling test

	sessionID := uuid.New()
	if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.CreateSession(ctx, tx, store.Session{
			SessionID: sessionID, SessionKey: sessionID.String(), TenantID: tenantID,
			SurfaceID: "test", UserID: uuid.New(), AgentID: uuid.Nil, AgentVersion: 1,
			HarnessDigest: []byte("test-digest"), AutonomyLevel: "supervised",
		})
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	// A generous ceiling ($1.00) well above the ~$0.085 worst-case reservation
	// — this test is about Reconcile's UNREPORTED handling, not refusal.
	if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := cost.CreateBudget(ctx, tx, tenantID, cost.BudgetScopeSession, &sessionID, cost.Money{Micros: 1_000_000, Currency: cost.DefaultCurrency})
		return err
	}); err != nil {
		t.Fatalf("create session budget: %v", err)
	}

	fakeProvider := fake.New(fake.Script{
		Chunks:    []fake.ChunkSpec{{Kind: "content", Text: "partial output"}, {Kind: "content", Text: "never arrives"}},
		Malformed: true, // the second (last) chunk becomes a stream error instead — no usage chunk is ever emitted
	})

	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fakeProvider}),
		Tools:    kernel.NotImplementedToolExecutor{},
		Budget:   gate,
		Store:    st,
	}
	rst := &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: identitySeal}
	for _, err := range k.Run(ctx, rst, kernel.RunConfig{System: "test agent", Input: "go", MaxTurns: 5}) {
		if err != nil {
			t.Fatalf("kernel.Run yielded an error: %v", err)
		}
	}

	sess, err := func() (store.Session, error) {
		var s store.Session
		err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var gerr error
			s, gerr = store.GetSession(ctx, tx, sessionID)
			return gerr
		})
		return s, err
	}()
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.TerminalReason == nil || *sess.TerminalReason != "error" {
		t.Fatalf("terminal_reason = %v, want \"error\" (a malformed stream is a permanent, unretried failure)", sess.TerminalReason)
	}

	var meter string
	var quantity, minorUnits int64
	var unreported bool
	if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT meter, quantity, minor_units, unreported FROM cost_records WHERE session_id = $1`, sessionID).
			Scan(&meter, &quantity, &minorUnits, &unreported)
	}); err != nil {
		t.Fatalf("query cost_records: %v", err)
	}
	if !unreported {
		t.Error("cost_records row is not flagged unreported")
	}
	if meter != string(cost.MeterUnreportedReservation) {
		t.Errorf("meter = %q, want %q", meter, cost.MeterUnreportedReservation)
	}
	if minorUnits != perCallMicros {
		t.Errorf("minor_units = %d, want the full reserved worst case %d (an unreliable provider must not look free)", minorUnits, perCallMicros)
	}
}
