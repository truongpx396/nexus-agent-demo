package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Lineage reads and records a session's place in the spawn tree inside the
// caller's transaction. The session_lineage table belongs to internal/delegate, so
// its implementation (delegate/postgres.LineageOf and RecordLineage) is injected
// rather than imported: this adapter does not reach into another feature's
// adapter.
type Lineage struct {
	Of     func(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (root uuid.UUID, depth int, err error)
	Record func(ctx context.Context, tx pgx.Tx, tenantID, sessionID, root uuid.UUID, depth int) error
}

func (t *txn) LineageOf(ctx context.Context, sessionID uuid.UUID) (uuid.UUID, int, error) {
	if t.lineage.Of == nil {
		return uuid.Nil, 0, errors.New("teams: no spawn-tree reader is wired")
	}
	return t.lineage.Of(ctx, t.Tx(), sessionID)
}

func (t *txn) RecordLineage(ctx context.Context, tenantID, sessionID, root uuid.UUID, depth int) error {
	if t.lineage.Record == nil {
		return errors.New("teams: no spawn-tree writer is wired")
	}
	return t.lineage.Record(ctx, t.Tx(), tenantID, sessionID, root, depth)
}
