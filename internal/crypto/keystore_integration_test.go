//go:build integration

package crypto

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// setupKeystoreEnv starts a single postgres container, applies migrations
// as the superuser, and returns a pool connected directly as nexus_app —
// the non-superuser role encryption_keys' RLS policy actually restricts
// (see internal/store's isolation test and migrations/0000_app_role.sql for
// why connecting as the superuser migration role would prove nothing).
// PgBouncer is not needed here: this test is about per-tenant key isolation
// and shredding, not about the transaction-pooling behavior the isolation
// test covers.
func setupKeystoreEnv(t *testing.T) *pgxpool.Pool {
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
	pgC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: pgReq,
		Started:          true,
	})
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
	if _, err := store.Migrate(ctx, migratePool, migrations.FS); err != nil {
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

func TestKeyStore_NewDEKThenUnwrapRoundTrip(t *testing.T) {
	pool := setupKeystoreEnv(t)
	ctx := context.Background()

	kek, err := GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	ks := NewKeyStore(kek)
	s := store.New(pool)
	tenantID := uuid.New()

	if err := s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'kt')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	var dek DEK
	if err := s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		dek, err = ks.NewDEK(ctx, tx, tenantID)
		return err
	}); err != nil {
		t.Fatalf("NewDEK: %v", err)
	}
	if dek.KeyID == "" || len(dek.Key) == 0 {
		t.Fatalf("NewDEK returned an unusable DEK: %+v", dek)
	}

	// Seal/Open with the freshly minted DEK, proving it is immediately
	// usable without a round trip to reload it.
	sealed, err := Seal(dek, []byte("hello"), tenantID.String(), "session-1")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	var reloaded DEK
	if err := s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reloaded, err = ks.Unwrap(ctx, tx, dek.KeyID)
		return err
	}); err != nil {
		t.Fatalf("Unwrap: %v", err)
	}

	got, err := Open(reloaded, sealed, tenantID.String(), "session-1")
	if err != nil {
		t.Fatalf("Open with reloaded DEK: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestKeyStore_UnwrapUnderWrongTenantIsNotFound(t *testing.T) {
	pool := setupKeystoreEnv(t)
	ctx := context.Background()

	kek, err := GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	ks := NewKeyStore(kek)
	s := store.New(pool)

	owner := uuid.New()
	other := uuid.New()
	for _, id := range []uuid.UUID{owner, other} {
		if err := s.InTenantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'kt')`, id)
			return err
		}); err != nil {
			t.Fatalf("insert tenant %s: %v", id, err)
		}
	}

	var keyID string
	if err := s.InTenantTx(ctx, owner, func(ctx context.Context, tx pgx.Tx) error {
		dek, err := ks.NewDEK(ctx, tx, owner)
		keyID = dek.KeyID
		return err
	}); err != nil {
		t.Fatalf("NewDEK: %v", err)
	}

	// RLS on encryption_keys must make owner's key invisible to a
	// transaction scoped to a different tenant — Unwrap must fail, not
	// silently succeed with someone else's key.
	err = s.InTenantTx(ctx, other, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ks.Unwrap(ctx, tx, keyID)
		return err
	})
	if err == nil {
		t.Fatal("Unwrap succeeded across tenant scope — RLS did not isolate encryption_keys")
	}
}

func TestKeyStore_UnwrapAfterShredFails(t *testing.T) {
	pool := setupKeystoreEnv(t)
	ctx := context.Background()

	kek, err := GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	ks := NewKeyStore(kek)
	s := store.New(pool)
	tenantID := uuid.New()

	if err := s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'kt')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	var keyID string
	if err := s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		dek, err := ks.NewDEK(ctx, tx, tenantID)
		keyID = dek.KeyID
		return err
	}); err != nil {
		t.Fatalf("NewDEK: %v", err)
	}

	// Phase 5 implements the real shredding path (internal/crypto/shred.go);
	// this test only needs the status flip Unwrap already reacts to.
	if err := s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE encryption_keys SET status = 'shredded', shredded_at = now() WHERE key_id = $1`, keyID)
		return err
	}); err != nil {
		t.Fatalf("mark shredded: %v", err)
	}

	err = s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ks.Unwrap(ctx, tx, keyID)
		return err
	})
	if !errors.Is(err, ErrKeyShredded) {
		t.Fatalf("err = %v, want ErrKeyShredded", err)
	}
}
