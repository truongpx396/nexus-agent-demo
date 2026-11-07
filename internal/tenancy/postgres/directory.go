// Package postgres is the Postgres adapter for tenancy.Directory.
package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/nexus-agent-demo/internal/tenancy"
)

// Directory implements tenancy.Directory over the tenants table.
//
// The runtime role (nexus_app, through PgBouncer) is RLS-restricted to whatever
// app.tenant_id the current transaction set, and there is no tenant to scope to
// when the question is "which tenants exist". So this connects directly as the
// administrative role, like the migrate command does for admin-only DDL. It
// opens a pool per call: the passes that use it run every few minutes, and
// holding an admin connection open for the life of the process is the worse
// trade.
type Directory struct {
	// DSN is the administrative connection string.
	DSN string
}

var _ tenancy.Directory = Directory{}

func (d Directory) ListTenantIDs(ctx context.Context) ([]uuid.UUID, error) {
	pool, err := pgxpool.New(ctx, d.DSN)
	if err != nil {
		return nil, fmt.Errorf("tenancy: connect as admin: %w", err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, `SELECT tenant_id FROM tenants`)
	if err != nil {
		return nil, fmt.Errorf("tenancy: list tenants: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("tenancy: scan tenant id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
