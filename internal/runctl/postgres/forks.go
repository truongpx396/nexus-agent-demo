package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// RecordFork writes the fork's provenance: which session it was taken from, at
// which event, and what it changed. session_forks is runctl's own table; the
// session row itself says nothing about being a fork.
func (t *txn) RecordFork(ctx context.Context, tenantID, sessionID, parentSessionID uuid.UUID, atSeq int64, overrides map[string]string) error {
	raw, err := json.Marshal(overrides)
	if err != nil {
		return fmt.Errorf("runctl: marshal fork overrides: %w", err)
	}
	if _, err := t.Tx().Exec(ctx, `
		INSERT INTO session_forks (session_id, tenant_id, forked_from_session_id, fork_seq, overrides)
		VALUES ($1, $2, $3, $4, $5)`,
		sessionID, tenantID, parentSessionID, atSeq, raw,
	); err != nil {
		return fmt.Errorf("runctl: record fork %s of %s: %w", sessionID, parentSessionID, err)
	}
	return nil
}
