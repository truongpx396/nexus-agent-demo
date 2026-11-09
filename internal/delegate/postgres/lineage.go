package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LineageOf reads where sessionID sits in the spawn tree: the root session it
// descends from and how many spawns deep it is. A session with no record was not
// spawned, so it is its own root at depth 0. It does not check that the session
// exists; a caller that needs that asks the session aggregate.
//
// It and RecordLineage are transaction-level functions, like CreateEnvelope,
// because internal/teams spawns sessions too and has to read and write the same
// tree inside its own transaction. cmd/nexusd injects them into the teams
// adapter, which does not import this one.
func LineageOf(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (root uuid.UUID, depth int, err error) {
	err = tx.QueryRow(ctx,
		`SELECT root_session_id, depth FROM session_lineage WHERE session_id = $1`, sessionID,
	).Scan(&root, &depth)
	if errors.Is(err, pgx.ErrNoRows) {
		return sessionID, 0, nil
	}
	if err != nil {
		return uuid.Nil, 0, fmt.Errorf("delegate: read lineage of %s: %w", sessionID, err)
	}
	return root, depth, nil
}

// RecordLineage places a freshly spawned session under root at depth (at least 1:
// a root has no record).
func RecordLineage(ctx context.Context, tx pgx.Tx, tenantID, sessionID, root uuid.UUID, depth int) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO session_lineage (session_id, tenant_id, root_session_id, depth) VALUES ($1, $2, $3, $4)`,
		sessionID, tenantID, root, depth,
	); err != nil {
		return fmt.Errorf("delegate: record lineage of %s: %w", sessionID, err)
	}
	return nil
}
