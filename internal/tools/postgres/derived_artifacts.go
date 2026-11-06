// Package postgres is the Postgres adapter for the tool pipeline's durable
// side: the derived_artifacts rows it records when a tool spills an output to
// the blob store.
package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// NewDerivedArtifactRecorder returns a tools.DerivedArtifactRecorder that
// writes a derived_artifacts row (README task 5.4). It is best-effort and runs
// in its own small transaction, not the caller's: crypto-shredding's
// ReconcileDerivedArtifacts (internal/crypto/postgres) is the backstop for
// whatever this misses.
func NewDerivedArtifactRecorder(st *store.Store) tools.DerivedArtifactRecorder {
	return func(ctx context.Context, tenantID, sessionID uuid.UUID, kind, path string) error {
		return st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO derived_artifacts (artifact_id, tenant_id, session_id, kind, path) VALUES ($1,$2,$3,$4,$5)`,
				uuid.New(), tenantID, sessionID, kind, path,
			)
			return err
		})
	}
}
