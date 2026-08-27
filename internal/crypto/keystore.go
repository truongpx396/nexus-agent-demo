package crypto

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrKeyShredded is returned by Unwrap when the requested key has been
// crypto-shredded (Phase 5): the row still exists so the audit chain and any
// digest referencing it remain intact, but the key material is gone and can
// never be recovered.
var ErrKeyShredded = errors.New("encryption key has been shredded")

// KeyStore persists wrapped per-tenant DEKs in encryption_keys. Every method
// must run inside a tenant-scoped transaction (store.Store.InTenantTx) — the
// table is RLS'd like every other tenant-owned row.
type KeyStore struct {
	KEK KEK
}

func NewKeyStore(kek KEK) *KeyStore {
	return &KeyStore{KEK: kek}
}

// NewDEK generates a fresh DEK for tenantID, persists its wrapped form, and
// returns the usable (unwrapped) key for immediate use sealing this turn's
// events.
func (ks *KeyStore) NewDEK(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (DEK, error) {
	keyID := uuid.New().String()
	dek, err := GenerateDEK(keyID)
	if err != nil {
		return DEK{}, err
	}
	wrapped, err := WrapDEK(ks.KEK, dek)
	if err != nil {
		return DEK{}, fmt.Errorf("wrap new DEK: %w", err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO encryption_keys (key_id, tenant_id, wrapped_dek) VALUES ($1, $2, $3)`,
		keyID, tenantID, wrapped,
	)
	if err != nil {
		return DEK{}, fmt.Errorf("persist wrapped DEK: %w", err)
	}
	return dek, nil
}

// Unwrap loads and unwraps the DEK identified by keyID, within the caller's
// tenant-scoped transaction (RLS makes a cross-tenant keyID invisible rather
// than merely refused).
func (ks *KeyStore) Unwrap(ctx context.Context, tx pgx.Tx, keyID string) (DEK, error) {
	var wrapped []byte
	var status string
	err := tx.QueryRow(ctx,
		`SELECT wrapped_dek, status FROM encryption_keys WHERE key_id = $1`,
		keyID,
	).Scan(&wrapped, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DEK{}, fmt.Errorf("encryption key %s not found (wrong tenant scope, or it does not exist)", keyID)
		}
		return DEK{}, fmt.Errorf("load encryption key %s: %w", keyID, err)
	}
	if status == "shredded" {
		return DEK{}, ErrKeyShredded
	}
	return UnwrapDEK(ks.KEK, keyID, wrapped)
}
