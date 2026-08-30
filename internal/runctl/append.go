package runctl

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// activeKeyID and appendEvent are near-identical copies of
// internal/oversight's own same-named functions (oversight/events.go) — see
// this package's own doc comment for why duplicating rather than importing
// is the right call here: every session in this demo has exactly one DEK
// for its whole lifetime, so "the session's most recent non-erasure key_id"
// is still the correct, and only, notion of "the current key."
func activeKeyID(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (string, error) {
	var keyID string
	err := tx.QueryRow(ctx,
		`SELECT key_id FROM events WHERE session_id = $1 AND key_id != $2 ORDER BY seq DESC LIMIT 1`,
		sessionID, crypto.ErasureKeyID,
	).Scan(&keyID)
	if err != nil {
		return "", fmt.Errorf("runctl: find active key for session %s: %w", sessionID, err)
	}
	return keyID, nil
}

// appendEvent seals payload under sessionID's active DEK and durably
// appends it, chaining a receipt when d.Chain is set — the same
// marshal -> seal -> append -> chain sequence kernel/loop.go's own
// appendEvent and internal/oversight's own appendEvent both run, for the
// same reason oversight's needs its own copy: this package operates OUT OF
// BAND, with no live kernel.RunState.Seal closure to reuse.
func (d deps) appendEvent(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, typ store.EventType, toolID *string, pairRef *uuid.UUID, payload any) (store.Event, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return store.Event{}, fmt.Errorf("runctl: marshal %s payload: %w", typ, err)
	}
	keyID, err := activeKeyID(ctx, tx, sessionID)
	if err != nil {
		return store.Event{}, err
	}
	dek, err := d.Keys.Unwrap(ctx, tx, keyID)
	if err != nil {
		return store.Event{}, fmt.Errorf("runctl: unwrap key for session %s: %w", sessionID, err)
	}
	sealed, err := crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
	if err != nil {
		return store.Event{}, fmt.Errorf("runctl: seal %s payload: %w", typ, err)
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
		return store.Event{}, fmt.Errorf("runctl: append %s: %w", typ, err)
	}
	if d.Chain != nil {
		if _, err := d.Chain.Append(ctx, tx, tenantID, sessionID, out.Seq, out.EventID, string(out.Type), out.PayloadDigest); err != nil {
			return store.Event{}, fmt.Errorf("runctl: chain %s: %w", typ, err)
		}
	}
	return out, nil
}
