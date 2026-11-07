package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Shred marks one key permanently, irreversibly unusable — the erasure act
// itself (README.md §5, task 5.4, FR-080): the row and its wrapped
// ciphertext stay (so a verifier can prove a key was destroyed, not merely
// forgotten), but Unwrap (internal/crypto/postgres/keystore.go) now always returns
// crypto.ErrKeyShredded for it. This is exactly the status flip
// keystore_integration_test.go's TestKeyStore_UnwrapAfterShredFails already
// exercised by hand, promoted to the real implementation it named.
func Shred(ctx context.Context, tx pgx.Tx, keyID string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE encryption_keys SET status = 'shredded', shredded_at = now() WHERE key_id = $1 AND status = 'active'`,
		keyID,
	)
	if err != nil {
		return fmt.Errorf("crypto: shred key %s: %w", keyID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("crypto: key %s not found (wrong tenant scope) or already shredded", keyID)
	}
	return nil
}

// ShredTenant shreds every active key belonging to tenantID, returning the
// key IDs it shredded. handleCreateRun (internal/runs/rest/run_create.go)
// mints one fresh crypto.DEK per SESSION, not one shared per tenant, so tenant-wide
// erasure is "every session's key," not a single row.
func ShredTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]string, error) {
	rows, err := tx.Query(ctx,
		`UPDATE encryption_keys SET status = 'shredded', shredded_at = now()
		 WHERE tenant_id = $1 AND status = 'active' RETURNING key_id`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("crypto: shred tenant %s: %w", tenantID, err)
	}
	defer rows.Close()
	var keyIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("crypto: scan shredded key id: %w", err)
		}
		keyIDs = append(keyIDs, id)
	}
	return keyIDs, rows.Err()
}

func sessionsForKeys(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, keyIDs []string) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT session_id FROM events WHERE tenant_id = $1 AND key_id = ANY($2::text[])`,
		tenantID, keyIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("crypto: find sessions for tenant %s: %w", tenantID, err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("crypto: scan session id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
