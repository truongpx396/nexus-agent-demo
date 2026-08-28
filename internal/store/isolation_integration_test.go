//go:build integration

package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// This file is the single most load-bearing test in the whole plan
// (README.md §5, task 1.4): it runs through a REAL PgBouncer in
// transaction-pooling mode, exactly like production, because a test against
// a direct Postgres connection proves nothing about the deployed topology —
// PgBouncer's transaction pooling is precisely what reassigns a physical
// backend connection between unrelated tenants between statements.

// setupIsolationEnv starts postgres + pgbouncer (transaction pooling) on a
// shared Docker network, applies migrations directly against postgres, and
// returns a fresh pgxpool connected THROUGH pgbouncer with the given max
// pool size — the only knob each test needs to vary.
func setupIsolationEnv(t *testing.T, appPoolMaxConns int32) (*pgxpool.Pool, func()) {
	t.Helper()
	ctx := context.Background()

	nw, err := tcnetwork.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}

	pgReq := testcontainers.ContainerRequest{
		Image:        "postgres:17",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "nexus",
			"POSTGRES_PASSWORD": "nexus",
			"POSTGRES_DB":       "nexus",
		},
		Networks:       []string{nw.Name},
		NetworkAliases: map[string][]string{nw.Name: {"postgres"}},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(120 * time.Second),
	}
	pgC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: pgReq,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}

	pbReq := testcontainers.ContainerRequest{
		Image:        "edoburu/pgbouncer:latest",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"DATABASE_URL":      "postgres://nexus:nexus@postgres:5432/nexus",
			"POOL_MODE":         "transaction",
			"AUTH_TYPE":         "scram-sha-256",
			"MAX_CLIENT_CONN":   "200",
			"DEFAULT_POOL_SIZE": "20",
		},
		Networks:   []string{nw.Name},
		WaitingFor: wait.ForLog("process up").WithStartupTimeout(60 * time.Second),
	}
	pbC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: pbReq,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start pgbouncer container: %v", err)
	}

	pgHost, err := pgC.Host(ctx)
	if err != nil {
		t.Fatalf("postgres host: %v", err)
	}
	pgPort, err := pgC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("postgres mapped port: %v", err)
	}
	pbHost, err := pbC.Host(ctx)
	if err != nil {
		t.Fatalf("pgbouncer host: %v", err)
	}
	pbPort, err := pbC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("pgbouncer mapped port: %v", err)
	}

	migrateDSN := fmt.Sprintf("postgres://nexus:nexus@%s:%s/nexus", pgHost, pgPort.Port())
	// nexus_app, not nexus: nexus is the migration role and a Postgres
	// superuser, which bypasses RLS unconditionally — testing isolation
	// while connected as a superuser would prove nothing (see
	// migrations/0000_app_role.sql).
	appDSN := fmt.Sprintf("postgres://nexus_app:nexus_app@%s:%s/nexus", pbHost, pbPort.Port())

	migratePool, err := pgxpool.New(ctx, migrateDSN)
	if err != nil {
		t.Fatalf("connect direct to postgres: %v", err)
	}
	if _, err := Migrate(ctx, migratePool, migrations.FS); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	appCfg, err := pgxpool.ParseConfig(appDSN)
	if err != nil {
		t.Fatalf("parse app DSN: %v", err)
	}
	appCfg.MaxConns = appPoolMaxConns
	appPool, err := pgxpool.NewWithConfig(ctx, appCfg)
	if err != nil {
		t.Fatalf("connect through pgbouncer: %v", err)
	}

	cleanup := func() {
		appPool.Close()
		migratePool.Close()
		_ = pbC.Terminate(ctx)
		_ = pgC.Terminate(ctx)
		_ = nw.Remove(ctx)
	}
	return appPool, cleanup
}

func insertTenant(ctx context.Context, s *Store, tenantID uuid.UUID, name string) error {
	return s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, $2)`, tenantID, name)
		return err
	})
}

func insertSession(ctx context.Context, s *Store, tenantID, sessionID uuid.UUID) error {
	return s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO sessions (
				session_id, session_key, tenant_id, surface_id, user_id,
				agent_id, agent_version, harness_digest, root_session_id
			) VALUES ($1, $2, $3, 'test', $4, $4, 1, '\x00', $1)`,
			sessionID, sessionID.String(), tenantID, uuid.Nil,
		)
		return err
	})
}

// TestIsolation_TransactionLocalScopeHoldsUnderConcurrency is the positive
// case: using ONLY the sanctioned InTenantTx path, many interleaved
// tenant-scoped transactions over a pool with room to reuse connections
// concurrently must never see another tenant's row — through the real
// PgBouncer transaction-pooling tier, under -race.
func TestIsolation_TransactionLocalScopeHoldsUnderConcurrency(t *testing.T) {
	pool, cleanup := setupIsolationEnv(t, 5)
	defer cleanup()
	ctx := context.Background()
	s := New(pool)

	tenantA := uuid.New()
	tenantB := uuid.New()
	if err := insertTenant(ctx, s, tenantA, "tenant-a"); err != nil {
		t.Fatalf("insert tenant A: %v", err)
	}
	if err := insertTenant(ctx, s, tenantB, "tenant-b"); err != nil {
		t.Fatalf("insert tenant B: %v", err)
	}
	if err := insertSession(ctx, s, tenantA, uuid.New()); err != nil {
		t.Fatalf("insert session for tenant A: %v", err)
	}
	if err := insertSession(ctx, s, tenantB, uuid.New()); err != nil {
		t.Fatalf("insert session for tenant B: %v", err)
	}

	const goroutines = 10
	const iterationsEach = 20

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*iterationsEach)
	for g := 0; g < goroutines; g++ {
		tenantID := tenantA
		if g%2 == 1 {
			tenantID = tenantB
		}
		wg.Add(1)
		go func(tenantID uuid.UUID) {
			defer wg.Done()
			for i := 0; i < iterationsEach; i++ {
				var count int
				err := s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&count)
				})
				if err != nil {
					errCh <- fmt.Errorf("query under tenant %s: %w", tenantID, err)
					return
				}
				if count != 1 {
					errCh <- fmt.Errorf("tenant %s saw %d sessions, want exactly 1 (its own) — cross-tenant leak", tenantID, count)
					return
				}
			}
		}(tenantID)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}
}

// TestIsolation_SessionLevelScopeLeaksAcrossTenants is the demonstration:
// it deliberately constructs the session-level variant Principle VI
// forbids (scopeTenant(..., local=false), i.e. flipping InTenantTx's one
// hardcoded boolean) and proves — through the SAME PgBouncer
// transaction-pooling tier, with the pool capped at exactly one connection
// so the leak is deterministic rather than merely possible — that a
// transaction which never scopes itself at all inherits the PREVIOUS
// transaction's tenant. This is why InTenantTx exposes no such parameter.
func TestIsolation_SessionLevelScopeLeaksAcrossTenants(t *testing.T) {
	pool, cleanup := setupIsolationEnv(t, 1)
	defer cleanup()
	ctx := context.Background()
	s := New(pool)

	tenantA := uuid.New()
	tenantB := uuid.New()
	if err := insertTenant(ctx, s, tenantA, "tenant-a"); err != nil {
		t.Fatalf("insert tenant A: %v", err)
	}
	if err := insertTenant(ctx, s, tenantB, "tenant-b"); err != nil {
		t.Fatalf("insert tenant B: %v", err)
	}
	if err := insertSession(ctx, s, tenantA, uuid.New()); err != nil {
		t.Fatalf("insert session for tenant A: %v", err)
	}
	// Tenant B deliberately gets NO session row — if the later unscoped
	// transaction reads tenant A's row anyway, that is unambiguously a leak
	// and not a coincidence of shared fixture data.

	// Step 1: scope a transaction to tenant A using the FORBIDDEN
	// session-level form (local=false) and commit. Because PgBouncer's
	// transaction-pooling mode does not reset session state between
	// transactions by default, app.tenant_id='tenant A' now persists on
	// whichever backend connection served this transaction.
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	if err := scopeTenant(ctx, tx1, tenantA, false /* the forbidden form */); err != nil {
		t.Fatalf("scopeTenant(local=false): %v", err)
	}
	if _, err := tx1.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("tx1 exec: %v", err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}

	// Step 2: a SECOND, completely independent transaction that never calls
	// scopeTenant at all — simulating a code path that forgot to, or wrongly
	// assumed a fresh connection starts with no prior tenant context. With
	// the pool capped at 1 connection, this is extremely likely (in
	// practice, deterministically) served by the very backend connection
	// tx1 just used.
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer tx2.Rollback(ctx) //nolint:errcheck // read-only; rollback is just cleanup

	var count int
	if err := tx2.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatalf("tx2 query: %v", err)
	}

	if count != 1 {
		t.Fatalf(
			"expected the leak to reproduce (tx2 sees tenant A's 1 session despite never scoping itself), "+
				"got count=%d instead — either the leak did not reproduce on this PgBouncer build/config, "+
				"or something upstream changed; this test exists to prove InTenantTx's local=true is not "+
				"a lint nit, so an unexpected result here is worth investigating, not silencing",
			count,
		)
	}
	t.Logf("confirmed: an unscoped transaction inherited tenant A's session-level scope and read %d row(s) it should never have been able to see — this is exactly what InTenantTx's local=true prevents", count)
}
