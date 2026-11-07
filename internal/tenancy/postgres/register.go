package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Register adds a tenant to the registry, in tx (which must already be scoped
// to id, since tenants is row-level secured like every table). It is idempotent:
// registering a tenant that exists changes nothing, so `nexusd seed` can be
// re-run.
func Register(ctx context.Context, tx pgx.Tx, id uuid.UUID, name string) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO tenants (tenant_id, name) VALUES ($1, $2) ON CONFLICT (tenant_id) DO NOTHING`,
		id, name,
	); err != nil {
		return fmt.Errorf("tenancy: register tenant %q: %w", name, err)
	}
	return nil
}
