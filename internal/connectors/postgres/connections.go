// Package postgres is the Postgres implementation of connectors.Connections:
// the oauth_connections table (migrations/0018_oauth_connections.sql), with the
// data key minted and unwrapped in the same tenant-scoped transaction.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/connectors"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	cryptopg "github.com/truongpx396/nexus-agent-demo/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Connections is connectors.Connections over Postgres.
type Connections struct {
	st   *store.Store
	keys *cryptopg.KeyStore
}

// NewConnections returns Connections over st, minting and unwrapping data keys
// with keys.
func NewConnections(st *store.Store, keys *cryptopg.KeyStore) *Connections {
	return &Connections{st: st, keys: keys}
}

var _ connectors.Connections = (*Connections)(nil)

// Save implements connectors.Connections.
func (c *Connections) Save(ctx context.Context, tenantID, userID uuid.UUID, provider string, expiresAt *time.Time,
	seal func(dek crypto.DEK) (access, refresh []byte, err error)) error {
	return c.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		dek, err := c.keys.NewDEK(ctx, tx, tenantID)
		if err != nil {
			return fmt.Errorf("mint DEK: %w", err)
		}
		access, refresh, err := seal(dek)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO oauth_connections (connection_id, tenant_id, user_id, provider, sealed_access_token, sealed_refresh_token, key_id, expires_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
			ON CONFLICT (tenant_id, user_id, provider) DO UPDATE
			SET sealed_access_token = EXCLUDED.sealed_access_token,
			    sealed_refresh_token = EXCLUDED.sealed_refresh_token,
			    key_id = EXCLUDED.key_id,
			    expires_at = EXCLUDED.expires_at,
			    updated_at = now()`,
			uuid.New(), tenantID, userID, provider, access, refresh, dek.KeyID, expiresAt,
		)
		if err != nil {
			return fmt.Errorf("upsert oauth_connections: %w", err)
		}
		return nil
	})
}

// Load implements connectors.Connections.
func (c *Connections) Load(ctx context.Context, tenantID, userID uuid.UUID, provider string) (connectors.StoredConnection, error) {
	var out connectors.StoredConnection
	err := c.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var keyID string
		err := tx.QueryRow(ctx,
			`SELECT sealed_access_token, sealed_refresh_token, key_id, expires_at FROM oauth_connections WHERE tenant_id = $1 AND user_id = $2 AND provider = $3`,
			tenantID, userID, provider,
		).Scan(&out.SealedAccess, &out.SealedRefresh, &keyID, &out.ExpiresAt)
		if err != nil {
			return fmt.Errorf("load connection: %w", err)
		}
		out.DEK, err = c.keys.Unwrap(ctx, tx, keyID)
		if err != nil {
			return fmt.Errorf("unwrap key: %w", err)
		}
		return nil
	})
	return out, err
}
