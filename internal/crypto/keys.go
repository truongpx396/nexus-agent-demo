package crypto

import (
	"context"

	"github.com/google/uuid"
)

// Keys mints and opens a tenant's data keys inside one tenant-scoped unit of
// work, so a key is minted in the same transaction as the session it will seal,
// or unwrapped in the same transaction as the events it opens. A service's own
// Tx interface embeds it. The Postgres implementation is
// internal/eventlog/sealed, over internal/crypto/postgres.KeyStore.
type Keys interface {
	// NewDEK mints a fresh data key for tenantID, stored wrapped under the KEK.
	NewDEK(ctx context.Context, tenantID uuid.UUID) (DEK, error)

	// Unwrap returns the data key named keyID.
	Unwrap(ctx context.Context, keyID string) (DEK, error)
}
