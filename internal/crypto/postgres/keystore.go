// Package postgres is the Postgres adapter for per-tenant data keys: the
// wrapped DEKs in encryption_keys and the crypto-shredding that makes erasure
// real. The envelope itself (DEK, KEK, Seal, Open) is internal/crypto, which
// has no storage.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
)

// KeyStore persists wrapped per-tenant DEKs in encryption_keys. Every method
// must run inside a tenant-scoped transaction (store.Store.InTenantTx) — the
// table is RLS'd like every other tenant-owned row.
type KeyStore struct {
	KEK crypto.KEK
}

func NewKeyStore(kek crypto.KEK) *KeyStore {
	return &KeyStore{KEK: kek}
}

// NewDEK generates a fresh crypto.DEK for tenantID, persists its wrapped form, and
// returns the usable (unwrapped) key for immediate use sealing this turn's
// events.
func (ks *KeyStore) NewDEK(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (crypto.DEK, error) {
	keyID := uuid.New().String()
	dek, err := crypto.GenerateDEK(keyID)
	if err != nil {
		return crypto.DEK{}, err
	}
	wrapped, err := crypto.WrapDEK(ks.KEK, dek)
	if err != nil {
		return crypto.DEK{}, fmt.Errorf("wrap new crypto.DEK: %w", err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO encryption_keys (key_id, tenant_id, wrapped_dek) VALUES ($1, $2, $3)`,
		keyID, tenantID, wrapped,
	)
	if err != nil {
		return crypto.DEK{}, fmt.Errorf("persist wrapped crypto.DEK: %w", err)
	}
	return dek, nil
}

// Unwrap loads and unwraps the crypto.DEK identified by keyID, within the caller's
// tenant-scoped transaction (RLS makes a cross-tenant keyID invisible rather
// than merely refused).
func (ks *KeyStore) Unwrap(ctx context.Context, tx pgx.Tx, keyID string) (crypto.DEK, error) {
	var wrapped []byte
	var status string
	err := tx.QueryRow(ctx,
		`SELECT wrapped_dek, status FROM encryption_keys WHERE key_id = $1`,
		keyID,
	).Scan(&wrapped, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return crypto.DEK{}, fmt.Errorf("encryption key %s not found (wrong tenant scope, or it does not exist)", keyID)
		}
		return crypto.DEK{}, fmt.Errorf("load encryption key %s: %w", keyID, err)
	}
	if status == "shredded" {
		return crypto.DEK{}, crypto.ErrKeyShredded
	}
	return crypto.UnwrapDEK(ks.KEK, keyID, wrapped)
}
