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
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// setupInboundDeliveryEnv mirrors internal/crypto/keystore_integration_test.go's
// own setupKeystoreEnv (this codebase's established per-file duplication
// idiom for integration test scaffolding not exercising PgBouncer itself).
func setupInboundDeliveryEnv(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	pgReq := testcontainers.ContainerRequest{
		Image:        "pgvector/pgvector:pg17",
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

	host, err := pgC.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := pgC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}

	migrateDSN := fmt.Sprintf("postgres://nexus:nexus@%s:%s/nexus", host, port.Port())
	migratePool, err := pgxpool.New(ctx, migrateDSN)
	if err != nil {
		t.Fatalf("connect as migration role: %v", err)
	}
	defer migratePool.Close()
	if _, err := Migrate(ctx, migratePool, migrations.FS); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	appDSN := fmt.Sprintf("postgres://nexus_app:nexus_app@%s:%s/nexus", host, port.Port())
	appPool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect as nexus_app: %v", err)
	}
	t.Cleanup(appPool.Close)
	return appPool
}

// TestClaimInboundDelivery_FirstCallerClaimsLaterCallersDoNot is the
// mechanism migrations/0024_inbound_deliveries.sql exists for: a provider
// redelivering the exact same (tenant, surface, delivery id) must never
// see a second claim, no matter how many times it retries.
func TestClaimInboundDelivery_FirstCallerClaimsLaterCallersDoNot(t *testing.T) {
	pool := setupInboundDeliveryEnv(t)
	s := New(pool)
	tenantID := uuid.New()
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'inbound-delivery-test')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	var results []bool
	for i := 0; i < 3; i++ {
		var claimed bool
		if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			claimed, err = ClaimInboundDelivery(ctx, tx, tenantID, "telegram", "update-1", "telegram:999")
			return err
		}); err != nil {
			t.Fatalf("claim attempt %d: %v", i, err)
		}
		results = append(results, claimed)
	}
	if results[0] != true {
		t.Fatalf("first claim = %v, want true", results[0])
	}
	if results[1] || results[2] {
		t.Fatalf("later claims = %v, want all false (same delivery id already claimed)", results[1:])
	}

	// A DIFFERENT delivery id for the same session is unaffected.
	var claimedOther bool
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		claimedOther, err = ClaimInboundDelivery(ctx, tx, tenantID, "telegram", "update-2", "telegram:999")
		return err
	}); err != nil {
		t.Fatalf("claim different delivery: %v", err)
	}
	if !claimedOther {
		t.Fatal("a different delivery id must claim independently")
	}
}

// TestClaimInboundDelivery_EmptyDeliveryIDNeverDedupes covers providers
// (Zalo's minimal event shape, a bare email relay with no Message-ID) whose
// inbound payload carries no stable id at all — every such call must claim,
// never silently collide on the empty string.
func TestClaimInboundDelivery_EmptyDeliveryIDNeverDedupes(t *testing.T) {
	pool := setupInboundDeliveryEnv(t)
	s := New(pool)
	tenantID := uuid.New()
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'inbound-delivery-test')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	for i := 0; i < 3; i++ {
		var claimed bool
		if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			claimed, err = ClaimInboundDelivery(ctx, tx, tenantID, "zalo", "", "zalo:abc")
			return err
		}); err != nil {
			t.Fatalf("claim attempt %d: %v", i, err)
		}
		if !claimed {
			t.Fatalf("attempt %d: claimed = false, want true (empty delivery id must never dedupe)", i)
		}
	}
}

// TestClaimInboundDelivery_ConcurrentCallersExactlyOneWins is the
// concurrency-under-real-Postgres proof the review this migration answers
// asked for directly: two goroutines racing the SAME delivery id must
// never both claim it.
func TestClaimInboundDelivery_ConcurrentCallersExactlyOneWins(t *testing.T) {
	pool := setupInboundDeliveryEnv(t)
	s := New(pool)
	tenantID := uuid.New()
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'inbound-delivery-test')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	claims := 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var claimed bool
			if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				claimed, err = ClaimInboundDelivery(ctx, tx, tenantID, "telegram", "racing-update", "telegram:999")
				return err
			}); err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if claimed {
				mu.Lock()
				claims++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claims != 1 {
		t.Fatalf("claims = %d across %d racing goroutines, want exactly 1", claims, racers)
	}
}
