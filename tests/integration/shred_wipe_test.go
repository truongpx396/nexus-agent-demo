//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	cryptopg "github.com/truongpx396/nexus-agent-demo/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

func wrappedLen(t *testing.T, ctx context.Context, st *store.Store, tenantID uuid.UUID, keyID string) (n int, status string) {
	t.Helper()
	err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT octet_length(wrapped_dek), status FROM encryption_keys WHERE key_id = $1`, keyID).Scan(&n, &status)
	})
	if err != nil {
		t.Fatalf("read key %s: %v", keyID, err)
	}
	return n, status
}

// Shredding is the erasure act, so it has to leave nothing to unwrap: the row stays
// as proof the key was retired, and the wrapped bytes do not.
func TestShred_EmptiesTheWrappedKeyForOneKeyAndForATenant(t *testing.T) {
	rig := setupOversightRig(t)
	ctx := context.Background()

	newKey := func() crypto.DEK {
		var dek crypto.DEK
		err := rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var kerr error
			dek, kerr = rig.keys.NewDEK(ctx, tx, rig.tenantID)
			return kerr
		})
		if err != nil {
			t.Fatalf("NewDEK: %v", err)
		}
		return dek
	}
	one, a, b := newKey(), newKey(), newKey()
	if n, status := wrappedLen(t, ctx, rig.st, rig.tenantID, one.KeyID); n == 0 || status != "active" {
		t.Fatalf("a live key has %d wrapped bytes and status %q, want bytes and active", n, status)
	}

	if err := rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return cryptopg.Shred(ctx, tx, one.KeyID)
	}); err != nil {
		t.Fatalf("Shred: %v", err)
	}
	if n, status := wrappedLen(t, ctx, rig.st, rig.tenantID, one.KeyID); n != 0 || status != "shredded" {
		t.Fatalf("after Shred: %d wrapped bytes, status %q, want none and shredded", n, status)
	}

	var shredded []string
	if err := rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var serr error
		shredded, serr = cryptopg.ShredTenant(ctx, tx, rig.tenantID)
		return serr
	}); err != nil || len(shredded) != 2 {
		t.Fatalf("ShredTenant: %v, shredded %v, want the two keys still active", err, shredded)
	}
	for _, id := range []string{a.KeyID, b.KeyID} {
		if n, status := wrappedLen(t, ctx, rig.st, rig.tenantID, id); n != 0 || status != "shredded" {
			t.Fatalf("after ShredTenant: key %s has %d wrapped bytes, status %q", id, n, status)
		}
	}

	err := rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, uerr := rig.keys.Unwrap(ctx, tx, one.KeyID)
		return uerr
	})
	if !errors.Is(err, crypto.ErrKeyShredded) {
		t.Fatalf("Unwrap of a shredded key = %v, want ErrKeyShredded", err)
	}
}

// Keys shredded before the wipe existed still hold their bytes. Migration 0030
// brings them to the same state and leaves live keys alone.
func TestMigration_WipesTheWrappedKeyOfAlreadyShreddedKeys(t *testing.T) {
	ctx := context.Background()
	pool := bareAdminPool(t)
	if _, err := store.Migrate(ctx, pool, migrationsUpTo(t, 29)); err != nil {
		t.Fatalf("migrate to the old shape: %v", err)
	}

	tenant := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'wipe-test')`, tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	live, old := uuid.New(), uuid.New()
	for _, k := range []struct {
		id     uuid.UUID
		status string
	}{{live, "active"}, {old, "shredded"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO encryption_keys (key_id, tenant_id, wrapped_dek, status) VALUES ($1, $2, '\xdeadbeef', $3)`, k.id, tenant, k.status); err != nil {
			t.Fatalf("seed key: %v", err)
		}
	}

	if _, err := store.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate forward: %v", err)
	}
	var liveLen, oldLen int
	if err := pool.QueryRow(ctx, `SELECT octet_length(wrapped_dek) FROM encryption_keys WHERE key_id = $1`, live).Scan(&liveLen); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT octet_length(wrapped_dek) FROM encryption_keys WHERE key_id = $1`, old).Scan(&oldLen); err != nil {
		t.Fatal(err)
	}
	if liveLen != 4 || oldLen != 0 {
		t.Fatalf("after the migration: live key %d bytes, shredded key %d bytes, want 4 and 0", liveLen, oldLen)
	}
}
