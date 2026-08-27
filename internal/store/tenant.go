package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the only sanctioned entry point onto tenant-scoped Postgres rows.
type Store struct {
	Pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool}
}

// InTenantTx is the ONLY sanctioned way to scope a database operation to a
// tenant (docs/constitution.md, Principle VI). It sets app.tenant_id
// transaction-locally and never at the session level, because PgBouncer's
// transaction-pooling tier reassigns a physical connection between tenants
// between statements — a session-level SET would leak scope onto whichever
// tenant's transaction happens to reuse that connection next.
func (s *Store) InTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := scopeTenant(ctx, tx, tenantID, true); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

// scopeTenant is unexported and takes `local` for exactly one reason: so the
// isolation test (tests/integration, white-box within this package) can
// construct the session-level variant Principle VI forbids and prove —
// through PgBouncer's transaction-pooling tier — that flipping this single
// boolean is a cross-tenant data leak, not a lint nit. Production code has
// no path to call this with local=false; InTenantTx above is the only
// exported entry point and it is hardcoded to true.
func scopeTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, local bool) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, $2)`, tenantID.String(), local); err != nil {
		return fmt.Errorf("scope tenant: %w", err)
	}
	return nil
}
