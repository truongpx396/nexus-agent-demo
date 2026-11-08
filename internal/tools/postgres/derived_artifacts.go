// Package postgres is the Postgres adapter for the tool pipeline's durable
// side: the derived_artifacts rows it records when a tool spills an output to
// the blob store, and the removal of them that erasure asks for.
package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// NewDerivedArtifactRecorder returns a tools.DerivedArtifactRecorder that
// writes a derived_artifacts row (README task 5.4). It is best-effort and runs
// in its own small transaction, not the caller's: crypto-shredding's
// Eraser.ReconcileDerivedArtifacts (internal/crypto/postgres) is the backstop
// for whatever this misses.
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

// DerivedArtifacts removes derived_artifacts rows on behalf of erasure. The
// table is this package's; the decision to erase, and which sessions, is
// crypto's (cryptopg.Eraser, whose ArtifactStore this satisfies; cmd/nexusd
// does the wiring), so crypto holds this and calls it inside its own transaction.
type DerivedArtifacts struct{}

// DeleteForSessions hard-deletes the sessions' artifacts and returns them.
func (DerivedArtifacts) DeleteForSessions(ctx context.Context, tx pgx.Tx, sessionIDs []uuid.UUID) ([]crypto.DerivedArtifact, error) {
	return scanArtifacts(tx.Query(ctx,
		`DELETE FROM derived_artifacts WHERE session_id = ANY($1::uuid[]) AND deleted_at IS NULL
		 RETURNING artifact_id, tenant_id, session_id, kind, path`,
		sessionIDs,
	))
}

// TombstoneForSessions marks the sessions' artifacts deleted and returns them.
func (DerivedArtifacts) TombstoneForSessions(ctx context.Context, tx pgx.Tx, sessionIDs []uuid.UUID) ([]crypto.DerivedArtifact, error) {
	return scanArtifacts(tx.Query(ctx,
		`UPDATE derived_artifacts SET deleted_at = now()
		 WHERE session_id = ANY($1::uuid[]) AND deleted_at IS NULL
		 RETURNING artifact_id, tenant_id, session_id, kind, path`,
		sessionIDs,
	))
}

func scanArtifacts(rows pgx.Rows, err error) ([]crypto.DerivedArtifact, error) {
	if err != nil {
		return nil, fmt.Errorf("derived artifacts: %w", err)
	}
	defer rows.Close()
	var out []crypto.DerivedArtifact
	for rows.Next() {
		var a crypto.DerivedArtifact
		if err := rows.Scan(&a.ArtifactID, &a.TenantID, &a.SessionID, &a.Kind, &a.Path); err != nil {
			return nil, fmt.Errorf("scan derived artifact: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
