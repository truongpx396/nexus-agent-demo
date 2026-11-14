package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditpg "github.com/truongpx396/nexus-agent/internal/audit/postgres"
	"github.com/truongpx396/nexus-agent/internal/crypto"
	"github.com/truongpx396/nexus-agent/internal/eventlog"
	"github.com/truongpx396/nexus-agent/internal/store"
)

// ArtifactStore is how erasure reaches the derived artifacts: plaintext files
// the tool pipeline spilled outside the encrypted log, whose table belongs to
// the tool pipeline's adapter (internal/tools/postgres). Erasure must remove
// them in the same transaction that shreds the keys, so cmd/nexusd hands this
// package that owner's writer rather than this package writing the table.
type ArtifactStore interface {
	// DeleteForSessions hard-deletes the artifacts of the sessions and returns
	// them, so the caller can unlink their files once the transaction commits.
	DeleteForSessions(ctx context.Context, tx pgx.Tx, sessionIDs []uuid.UUID) ([]crypto.DerivedArtifact, error)
	// TombstoneForSessions marks the sessions' artifacts deleted, keeping the
	// rows, and returns them.
	TombstoneForSessions(ctx context.Context, tx pgx.Tx, sessionIDs []uuid.UUID) ([]crypto.DerivedArtifact, error)
}

// errNoArtifacts is returned instead of erasing without removing the derived
// artifacts: an erasure that left plaintext behind would pass for a complete one.
var errNoArtifacts = errors.New("crypto: Eraser has no ArtifactStore, so erasure would leave derived artifacts behind")

// Eraser performs erasure: crypto-shredding the keys, removing what was derived
// from them, and recording that it happened.
type Eraser struct {
	// Chain extends the audit chain for each erasure event. Nil is valid and
	// means no receipt is written, for tests with no chain wired.
	Chain *auditpg.Chain
	// Artifacts removes the derived artifacts. Required: a nil Artifacts fails
	// every erase and reconcile with an error rather than skip them.
	Artifacts ArtifactStore
}

type erasurePayload struct {
	Reason         string   `json:"reason"`
	ShreddedKeyIDs []string `json:"shredded_key_ids"`
	ErasedAt       string   `json:"erased_at"`
}

// EraseTenant performs task 5.4's erasure transaction for every session
// belonging to tenantID: shred every active key, hard-delete every
// derived artifact for those sessions in the SAME transaction, and
// append one EventErasure per affected session (chained via Eraser.Chain, if
// non-nil — nil is valid for tests with no audit chain wired). The caller
// (store.Store.InTenantTx) commits tx; ReconcileDerivedArtifacts is the
// backstop proving no derived row outlives its source even so.
func (e Eraser) EraseTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, reason string) (crypto.ErasureResult, error) {
	if e.Artifacts == nil {
		return crypto.ErasureResult{}, errNoArtifacts
	}
	keyIDs, err := ShredTenant(ctx, tx, tenantID)
	if err != nil {
		return crypto.ErasureResult{}, err
	}
	if len(keyIDs) == 0 {
		return crypto.ErasureResult{}, nil // nothing active to erase — not an error, a no-op
	}

	sessionIDs, err := sessionsForKeys(ctx, tx, tenantID, keyIDs)
	if err != nil {
		return crypto.ErasureResult{}, err
	}
	return e.finish(ctx, tx, tenantID, sessionIDs, keyIDs, reason)
}

// EraseSession performs task 5.4's erasure transaction for exactly one
// session: shred whichever key(s) sealed its events (ordinarily one), and
// the same in-transaction derived-artifact hard-delete + EventErasure as
// EraseTenant.
func (e Eraser) EraseSession(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, reason string) (crypto.ErasureResult, error) {
	if e.Artifacts == nil {
		return crypto.ErasureResult{}, errNoArtifacts
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT key_id FROM events WHERE session_id = $1 AND key_id != $2`, sessionID, crypto.ErasureKeyID)
	if err != nil {
		return crypto.ErasureResult{}, fmt.Errorf("crypto: find keys for session %s: %w", sessionID, err)
	}
	var candidateKeys []string
	for rows.Next() {
		var id string
		if serr := rows.Scan(&id); serr != nil {
			rows.Close()
			return crypto.ErasureResult{}, fmt.Errorf("crypto: scan key id: %w", serr)
		}
		candidateKeys = append(candidateKeys, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return crypto.ErasureResult{}, err
	}
	rows.Close()

	var shredded []string
	for _, keyID := range candidateKeys {
		if err := Shred(ctx, tx, keyID); err != nil {
			// Already shredded (e.g. a prior tenant-wide erasure already
			// got this key) is not an error worth failing the whole call
			// over — Shred's own "already shredded" case is the only
			// RowsAffected==0 path, and re-erasing an already-erased
			// session must stay idempotent.
			continue
		}
		shredded = append(shredded, keyID)
	}
	if len(shredded) == 0 {
		return crypto.ErasureResult{}, nil
	}
	return e.finish(ctx, tx, tenantID, []uuid.UUID{sessionID}, shredded, reason)
}

func (e Eraser) finish(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, sessionIDs []uuid.UUID, shreddedKeyIDs []string, reason string) (crypto.ErasureResult, error) {
	var artifacts []crypto.DerivedArtifact
	if len(sessionIDs) > 0 {
		var err error
		artifacts, err = e.Artifacts.DeleteForSessions(ctx, tx, sessionIDs)
		if err != nil {
			return crypto.ErasureResult{}, fmt.Errorf("crypto: delete derived artifacts: %w", err)
		}
	}

	result := crypto.ErasureResult{ShreddedKeyIDs: shreddedKeyIDs, DeletedArtifacts: artifacts}
	for _, sessionID := range sessionIDs {
		ev, err := appendErasureEvent(ctx, tx, e.Chain, tenantID, sessionID, shreddedKeyIDs, reason)
		if err != nil {
			return crypto.ErasureResult{}, err
		}
		result.ErasureEvents = append(result.ErasureEvents, ev)
	}
	return result, nil
}

func appendErasureEvent(ctx context.Context, tx pgx.Tx, chain *auditpg.Chain, tenantID, sessionID uuid.UUID, shreddedKeyIDs []string, reason string) (eventlog.Event, error) {
	plaintext, err := json.Marshal(erasurePayload{Reason: reason, ShreddedKeyIDs: shreddedKeyIDs, ErasedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return eventlog.Event{}, fmt.Errorf("crypto: marshal erasure payload: %w", err)
	}
	e := eventlog.Event{
		EventID:       uuid.New(),
		SessionID:     sessionID,
		TenantID:      tenantID,
		SchemaVersion: eventlog.CurrentSchemaVersion,
		Type:          eventlog.EventErasure,
		Payload:       plaintext, // plaintext, on purpose — see crypto.ErasureKeyID's doc comment
		PayloadDigest: crypto.Digest(plaintext),
		KeyID:         crypto.ErasureKeyID,
		Actor:         eventlog.ActorSystem,
	}
	out, err := store.Append(ctx, tx, e)
	if err != nil {
		return eventlog.Event{}, fmt.Errorf("crypto: append erasure event for session %s: %w", sessionID, err)
	}
	if chain != nil {
		if _, err := chain.Append(ctx, tx, tenantID, sessionID, out.Seq, out.EventID, string(out.Type), out.PayloadDigest); err != nil {
			return eventlog.Event{}, fmt.Errorf("crypto: chain erasure event for session %s: %w", sessionID, err)
		}
	}
	return out, nil
}

// ReconcileDerivedArtifacts is the backstop task 5.4 names: it finds any
// derived artifact EraseTenant/EraseSession's synchronous, same-transaction
// delete missed — one whose owning session's key(s) are ALL shredded, yet the
// artifact (and its file) still exists, e.g. because a blob spill raced an
// erasure that had already queried its session list. Unlike the synchronous
// path (a real DELETE, task 5.4's literal "hard-deleted"), the owner marks
// deleted_at rather than removing the row outright: by the time
// reconciliation runs, "in the same transaction as the erasure" no longer
// applies, and a tombstone proves the gap was found and closed rather than
// silently erasing evidence that it ever existed.
//
// Which sessions are fully shredded is this package's to say (it owns the
// keys); which artifacts hang off them is the artifact owner's.
func (e Eraser) ReconcileDerivedArtifacts(ctx context.Context, tx pgx.Tx) ([]crypto.DerivedArtifact, error) {
	if e.Artifacts == nil {
		return nil, errNoArtifacts
	}
	// encryption_keys.key_id is a uuid and events.key_id is text (it also holds
	// the non-uuid erasure marker), so the join compares them as text.
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT e.session_id FROM events e
		WHERE NOT EXISTS (
		  SELECT 1 FROM events e2
		  JOIN encryption_keys ek ON ek.key_id::text = e2.key_id
		  WHERE e2.session_id = e.session_id AND ek.status = 'active'
		)`)
	if err != nil {
		return nil, fmt.Errorf("crypto: find fully shredded sessions: %w", err)
	}
	defer rows.Close()
	var sessionIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("crypto: scan session id: %w", err)
		}
		sessionIDs = append(sessionIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	out, err := e.Artifacts.TombstoneForSessions(ctx, tx, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("crypto: reconcile derived artifacts: %w", err)
	}
	return out, nil
}
