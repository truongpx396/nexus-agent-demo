package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Snapshot is the artifact README task 6.4 names: a DISPOSABLE cache of one
// Projection at a given seq — never a source of truth, and never needed for
// correctness, only for speed. SaveSnapshot/LatestSnapshot/DeleteAll
// together are what task 6.4's own acceptance test exercises: computing a
// Projection via a cached Snapshot and via a from-scratch ReplayProjection
// must always agree, and deleting every Snapshot row must change nothing
// about that answer.
type Snapshot struct {
	SnapshotID     uuid.UUID
	TenantID       uuid.UUID
	SessionID      uuid.UUID
	AtSeq          int64
	Status         string
	TerminalReason *string
	CreatedAt      time.Time
}

// SaveSnapshot inserts a new snapshot row. Snapshots are append-only, like
// checkpoints — LatestSnapshot resolves the current one by AtSeq, and
// DeleteAllSnapshots is free to remove every row for a session without ever
// touching a session's real state (unlike DeleteAllSnapshots' non-existent
// Checkpoint counterpart: a checkpoint is NOT safe to bulk-delete, which is
// exactly what makes the two artifacts provably non-interchangeable).
func SaveSnapshot(ctx context.Context, tx pgx.Tx, s Snapshot) (Snapshot, error) {
	if s.SnapshotID == uuid.Nil {
		s.SnapshotID = uuid.New()
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO snapshots (snapshot_id, tenant_id, session_id, at_seq, status, terminal_reason)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING created_at`,
		s.SnapshotID, s.TenantID, s.SessionID, s.AtSeq, s.Status, s.TerminalReason,
	).Scan(&s.CreatedAt)
	if err != nil {
		return Snapshot{}, fmt.Errorf("save snapshot for session %s: %w", s.SessionID, err)
	}
	return s, nil
}

// LatestSnapshot returns sessionID's highest-AtSeq snapshot, if any — the
// fast path LoadProjection tries before falling back to a full replay.
func LatestSnapshot(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (Snapshot, bool, error) {
	row := tx.QueryRow(ctx, `
		SELECT snapshot_id, tenant_id, session_id, at_seq, status, terminal_reason, created_at
		FROM snapshots WHERE session_id = $1 ORDER BY at_seq DESC LIMIT 1`, sessionID)
	var s Snapshot
	err := row.Scan(&s.SnapshotID, &s.TenantID, &s.SessionID, &s.AtSeq, &s.Status, &s.TerminalReason, &s.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Snapshot{}, false, nil
		}
		return Snapshot{}, false, fmt.Errorf("latest snapshot for session %s: %w", sessionID, err)
	}
	return s, true, nil
}

// DeleteAllSnapshots removes every snapshot row for sessionID — the
// operation task 6.4's acceptance test performs before re-deriving the
// Projection from scratch and asserting the answer didn't move.
func DeleteAllSnapshots(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM snapshots WHERE session_id = $1`, sessionID); err != nil {
		return fmt.Errorf("delete snapshots for session %s: %w", sessionID, err)
	}
	return nil
}

// LoadProjection is the read path a caller actually uses: LatestSnapshot
// first (fast), ReplayProjection over the full history (slow but always
// correct) if none exists. It never writes a snapshot itself — that stays a
// deliberate, separate step (internal/runctl and internal/queue's worker
// decide WHEN a fresh snapshot is worth taking; LoadProjection only ever
// reads).
func LoadProjection(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (Projection, error) {
	snap, ok, err := LatestSnapshot(ctx, tx, sessionID)
	if err != nil {
		return Projection{}, err
	}
	if ok {
		return Projection{Status: snap.Status, TerminalReason: snap.TerminalReason}, nil
	}
	history, err := ListEvents(ctx, tx, sessionID)
	if err != nil {
		return Projection{}, err
	}
	return ReplayProjection(history), nil
}
