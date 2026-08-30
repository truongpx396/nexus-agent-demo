package crypto

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// ErasureKeyID is the sentinel Event.KeyID an EventErasure record carries.
// Erasure metadata (who/why/when) is not tenant-secret content the way an
// ordinary event's payload is — it is a structural, administrative fact,
// same spirit as internal/obs's "structure, not content" — so it is stored
// as PLAINTEXT directly in Payload rather than sealed under any DEK
// (there's no DEK left to seal it under once the tenant's key is shredded
// anyway). ErasureKeyID never names a real encryption_keys row; a reader
// checks for it before attempting KeyStore.Unwrap.
const ErasureKeyID = "erasure-record"

// Shred marks one key permanently, irreversibly unusable — the erasure act
// itself (README.md §5, task 5.4, FR-080): the row and its wrapped
// ciphertext stay (so a verifier can prove a key was destroyed, not merely
// forgotten), but Unwrap (internal/crypto/keystore.go) now always returns
// ErrKeyShredded for it. This is exactly the status flip
// keystore_integration_test.go's TestKeyStore_UnwrapAfterShredFails already
// exercised by hand, promoted to the real implementation it named.
func Shred(ctx context.Context, tx pgx.Tx, keyID string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE encryption_keys SET status = 'shredded', shredded_at = now() WHERE key_id = $1 AND status = 'active'`,
		keyID,
	)
	if err != nil {
		return fmt.Errorf("crypto: shred key %s: %w", keyID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("crypto: key %s not found (wrong tenant scope) or already shredded", keyID)
	}
	return nil
}

// ShredTenant shreds every active key belonging to tenantID, returning the
// key IDs it shredded. handleCreateRun (internal/surfaces/rest/server.go)
// mints one fresh DEK per SESSION, not one shared per tenant, so tenant-wide
// erasure is "every session's key," not a single row.
func ShredTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]string, error) {
	rows, err := tx.Query(ctx,
		`UPDATE encryption_keys SET status = 'shredded', shredded_at = now()
		 WHERE tenant_id = $1 AND status = 'active' RETURNING key_id`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("crypto: shred tenant %s: %w", tenantID, err)
	}
	defer rows.Close()
	var keyIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("crypto: scan shredded key id: %w", err)
		}
		keyIDs = append(keyIDs, id)
	}
	return keyIDs, rows.Err()
}

// DerivedArtifact mirrors one derived_artifacts row (migrations/
// 0008_derived_artifacts.sql) — a plaintext-derived file living outside the
// encrypted event log (today, only internal/tools.BlobStore's oversized-
// result spill files).
type DerivedArtifact struct {
	ArtifactID uuid.UUID
	TenantID   uuid.UUID
	SessionID  uuid.UUID
	Kind       string
	Path       string
}

// ErasureResult reports what one erasure transaction did. Callers are
// responsible for two post-commit, best-effort steps that cannot themselves
// be transactional with Postgres: unlinking DeletedArtifacts' Path values,
// and (for a caller that streams events to a client, e.g. an admin surface)
// forwarding ErasureEvents. ReconcileDerivedArtifacts is the backstop for
// whatever this best-effort step misses.
type ErasureResult struct {
	ShreddedKeyIDs   []string
	DeletedArtifacts []DerivedArtifact
	ErasureEvents    []store.Event
}

type erasurePayload struct {
	Reason         string   `json:"reason"`
	ShreddedKeyIDs []string `json:"shredded_key_ids"`
	ErasedAt       string   `json:"erased_at"`
}

// EraseTenant performs task 5.4's erasure transaction for every session
// belonging to tenantID: shred every active key, hard-delete every
// derived_artifacts row for those sessions in the SAME transaction, and
// append one EventErasure per affected session (chained via chain, if
// non-nil — nil is valid for tests with no audit chain wired). The caller
// (store.Store.InTenantTx) commits tx; ReconcileDerivedArtifacts is the
// backstop proving no derived row outlives its source even so.
func EraseTenant(ctx context.Context, tx pgx.Tx, chain *audit.Chain, tenantID uuid.UUID, reason string) (ErasureResult, error) {
	keyIDs, err := ShredTenant(ctx, tx, tenantID)
	if err != nil {
		return ErasureResult{}, err
	}
	if len(keyIDs) == 0 {
		return ErasureResult{}, nil // nothing active to erase — not an error, a no-op
	}

	sessionIDs, err := sessionsForKeys(ctx, tx, tenantID, keyIDs)
	if err != nil {
		return ErasureResult{}, err
	}
	return finishErasure(ctx, tx, chain, tenantID, sessionIDs, keyIDs, reason)
}

// EraseSession performs task 5.4's erasure transaction for exactly one
// session: shred whichever key(s) sealed its events (ordinarily one), and
// the same in-transaction derived-artifact hard-delete + EventErasure as
// EraseTenant.
func EraseSession(ctx context.Context, tx pgx.Tx, chain *audit.Chain, tenantID, sessionID uuid.UUID, reason string) (ErasureResult, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT key_id FROM events WHERE session_id = $1 AND key_id != $2`, sessionID, ErasureKeyID)
	if err != nil {
		return ErasureResult{}, fmt.Errorf("crypto: find keys for session %s: %w", sessionID, err)
	}
	var candidateKeys []string
	for rows.Next() {
		var id string
		if serr := rows.Scan(&id); serr != nil {
			rows.Close()
			return ErasureResult{}, fmt.Errorf("crypto: scan key id: %w", serr)
		}
		candidateKeys = append(candidateKeys, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ErasureResult{}, err
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
		return ErasureResult{}, nil
	}
	return finishErasure(ctx, tx, chain, tenantID, []uuid.UUID{sessionID}, shredded, reason)
}

func sessionsForKeys(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, keyIDs []string) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT session_id FROM events WHERE tenant_id = $1 AND key_id = ANY($2::text[])`,
		tenantID, keyIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("crypto: find sessions for tenant %s: %w", tenantID, err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("crypto: scan session id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func finishErasure(ctx context.Context, tx pgx.Tx, chain *audit.Chain, tenantID uuid.UUID, sessionIDs []uuid.UUID, shreddedKeyIDs []string, reason string) (ErasureResult, error) {
	artifacts, err := deleteDerivedArtifacts(ctx, tx, sessionIDs)
	if err != nil {
		return ErasureResult{}, err
	}

	result := ErasureResult{ShreddedKeyIDs: shreddedKeyIDs, DeletedArtifacts: artifacts}
	for _, sessionID := range sessionIDs {
		ev, err := appendErasureEvent(ctx, tx, chain, tenantID, sessionID, shreddedKeyIDs, reason)
		if err != nil {
			return ErasureResult{}, err
		}
		result.ErasureEvents = append(result.ErasureEvents, ev)
	}
	return result, nil
}

func deleteDerivedArtifacts(ctx context.Context, tx pgx.Tx, sessionIDs []uuid.UUID) ([]DerivedArtifact, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx,
		`DELETE FROM derived_artifacts WHERE session_id = ANY($1::uuid[]) AND deleted_at IS NULL
		 RETURNING artifact_id, tenant_id, session_id, kind, path`,
		sessionIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("crypto: delete derived artifacts: %w", err)
	}
	defer rows.Close()
	var out []DerivedArtifact
	for rows.Next() {
		var a DerivedArtifact
		if err := rows.Scan(&a.ArtifactID, &a.TenantID, &a.SessionID, &a.Kind, &a.Path); err != nil {
			return nil, fmt.Errorf("crypto: scan derived artifact: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func appendErasureEvent(ctx context.Context, tx pgx.Tx, chain *audit.Chain, tenantID, sessionID uuid.UUID, shreddedKeyIDs []string, reason string) (store.Event, error) {
	plaintext, err := json.Marshal(erasurePayload{Reason: reason, ShreddedKeyIDs: shreddedKeyIDs, ErasedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return store.Event{}, fmt.Errorf("crypto: marshal erasure payload: %w", err)
	}
	e := store.Event{
		EventID:       uuid.New(),
		SessionID:     sessionID,
		TenantID:      tenantID,
		SchemaVersion: store.CurrentSchemaVersion,
		Type:          store.EventErasure,
		Payload:       plaintext, // plaintext, on purpose — see ErasureKeyID's doc comment
		PayloadDigest: Digest(plaintext),
		KeyID:         ErasureKeyID,
		Actor:         store.ActorSystem,
	}
	out, err := store.Append(ctx, tx, e)
	if err != nil {
		return store.Event{}, fmt.Errorf("crypto: append erasure event for session %s: %w", sessionID, err)
	}
	if chain != nil {
		if _, err := chain.Append(ctx, tx, tenantID, sessionID, out.Seq, out.EventID, string(out.Type), out.PayloadDigest); err != nil {
			return store.Event{}, fmt.Errorf("crypto: chain erasure event for session %s: %w", sessionID, err)
		}
	}
	return out, nil
}

// ReconcileDerivedArtifacts is the backstop task 5.4 names: it finds any
// derived_artifacts row EraseTenant/EraseSession's synchronous, same-
// transaction delete missed — a row whose owning session's key(s) are ALL
// shredded, yet the row (and its file) still exists, e.g. because a blob
// spill raced an erasure that had already queried its session list. Unlike
// the synchronous path (a real DELETE, task 5.4's literal "hard-deleted"),
// this marks deleted_at rather than removing the row outright: by the time
// reconciliation runs, "in the same transaction as the erasure" no longer
// applies, and a tombstone proves the gap was found and closed rather than
// silently erasing evidence that it ever existed.
func ReconcileDerivedArtifacts(ctx context.Context, tx pgx.Tx) ([]DerivedArtifact, error) {
	rows, err := tx.Query(ctx, `
		UPDATE derived_artifacts da SET deleted_at = now()
		WHERE da.deleted_at IS NULL
		  AND EXISTS (SELECT 1 FROM events e WHERE e.session_id = da.session_id)
		  AND NOT EXISTS (
		    SELECT 1 FROM events e
		    JOIN encryption_keys ek ON ek.key_id = e.key_id
		    WHERE e.session_id = da.session_id AND ek.status = 'active'
		  )
		RETURNING artifact_id, tenant_id, session_id, kind, path`,
	)
	if err != nil {
		return nil, fmt.Errorf("crypto: reconcile derived artifacts: %w", err)
	}
	defer rows.Close()
	var out []DerivedArtifact
	for rows.Next() {
		var a DerivedArtifact
		if err := rows.Scan(&a.ArtifactID, &a.TenantID, &a.SessionID, &a.Kind, &a.Path); err != nil {
			return nil, fmt.Errorf("crypto: scan reconciled artifact: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
