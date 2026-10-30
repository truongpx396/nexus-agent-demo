// Package postgres holds the Postgres-backed adapters for internal/runs'
// driven ports.
package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	cryptopg "github.com/truongpx396/nexus-agent-demo/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Keys is the runs.KeyProvider over the tenant's wrapped-key table: each key
// is minted and persisted inside one tenant-scoped transaction.
type Keys struct {
	Store    *store.Store
	KeyStore *cryptopg.KeyStore
}

var _ runs.KeyProvider = Keys{}

func (k Keys) NewDEK(ctx context.Context, tenantID uuid.UUID) (crypto.DEK, error) {
	var dek crypto.DEK
	err := k.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		dek, derr = k.KeyStore.NewDEK(ctx, tx, tenantID)
		return derr
	})
	return dek, err
}
