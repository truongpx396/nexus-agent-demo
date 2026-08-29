package oversight

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// deps is the small set of collaborators every oversight component needs:
// durable storage, per-tenant key access (to seal a decision event under
// the session's own active DEK, the same way every other event in the
// session is sealed), and the audit chain (nil is valid — no receipt is
// written, matching kernel.Kernel.Receipts' own nil convention). Approvals
// and Inputs both embed this rather than duplicating the same three fields.
type deps struct {
	Store *store.Store
	Keys  *crypto.KeyStore
	Chain *audit.Chain
}

// activeKeyID finds the key_id a session's events are currently sealed
// under — its most recent event whose KeyID isn't crypto.ErasureKeyID
// (an erasure record is deliberately plaintext, never a real key). Every
// session in this demo has exactly one DEK for its whole lifetime
// (internal/surfaces/rest/server.go mints one per session, never rotates
// it), so this is equivalent to "the session's DEK," expressed without
// oversight needing its own separate notion of "the current key."
func activeKeyID(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (string, error) {
	var keyID string
	err := tx.QueryRow(ctx,
		`SELECT key_id FROM events WHERE session_id = $1 AND key_id != $2 ORDER BY seq DESC LIMIT 1`,
		sessionID, crypto.ErasureKeyID,
	).Scan(&keyID)
	if err != nil {
		return "", fmt.Errorf("oversight: find active key for session %s: %w", sessionID, err)
	}
	return keyID, nil
}

// appendEvent seals payload under sessionID's active DEK and durably
// appends it — the same marshal -> seal -> append -> chain sequence
// kernel/loop.go's own appendEvent runs, reimplemented here (rather than
// exported from kernel) because oversight operates OUT OF BAND, after a
// live kernel.Run/Resume call has already returned; it has no RunState.Seal
// closure to reuse.
func (d deps) appendEvent(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, typ store.EventType, toolID *string, pairRef *uuid.UUID, payload any) (store.Event, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return store.Event{}, fmt.Errorf("oversight: marshal %s payload: %w", typ, err)
	}
	keyID, err := activeKeyID(ctx, tx, sessionID)
	if err != nil {
		return store.Event{}, err
	}
	dek, err := d.Keys.Unwrap(ctx, tx, keyID)
	if err != nil {
		return store.Event{}, fmt.Errorf("oversight: unwrap key for session %s: %w", sessionID, err)
	}
	sealed, err := crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
	if err != nil {
		return store.Event{}, fmt.Errorf("oversight: seal %s payload: %w", typ, err)
	}

	e := store.Event{
		EventID:       uuid.New(),
		SessionID:     sessionID,
		TenantID:      tenantID,
		SchemaVersion: store.CurrentSchemaVersion,
		Type:          typ,
		Payload:       sealed,
		PayloadDigest: crypto.Digest(plaintext),
		KeyID:         dek.KeyID,
		Actor:         store.ActorSystem,
		ToolID:        toolID,
		PairRef:       pairRef,
	}
	out, err := store.Append(ctx, tx, e)
	if err != nil {
		return store.Event{}, fmt.Errorf("oversight: append %s: %w", typ, err)
	}
	if d.Chain != nil {
		if _, err := d.Chain.Append(ctx, tx, tenantID, sessionID, out.Seq, out.EventID, string(out.Type), out.PayloadDigest); err != nil {
			return store.Event{}, fmt.Errorf("oversight: chain %s: %w", typ, err)
		}
	}
	return out, nil
}
