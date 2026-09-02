package delegate

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

// deps is the small set of collaborators every delegate component needs —
// mirrors internal/oversight's own unexported `deps` type field-for-field
// (that package's own doc comment on why: two packages with no other reason
// to depend on each other).
type deps struct {
	Store *store.Store
	Keys  *crypto.KeyStore
	Chain *audit.Chain
}

func activeKeyID(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (string, error) {
	var keyID string
	err := tx.QueryRow(ctx,
		`SELECT key_id FROM events WHERE session_id = $1 AND key_id != $2 ORDER BY seq DESC LIMIT 1`,
		sessionID, crypto.ErasureKeyID,
	).Scan(&keyID)
	if err != nil {
		return "", fmt.Errorf("delegate: find active key for session %s: %w", sessionID, err)
	}
	return keyID, nil
}

// appendEvent seals payload under sessionID's active DEK and durably
// appends it — the marshal -> seal -> append -> chain sequence
// internal/oversight's own deps.appendEvent already runs, reimplemented
// here for the same out-of-band reason that file's doc comment gives: this
// package drives kernel.Kernel.ResumeDelegation from OUTSIDE any live
// kernel.Run call, with no RunState.Seal closure to reuse.
func (d deps) appendEvent(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, typ store.EventType, toolID *string, pairRef *uuid.UUID, payload any) (store.Event, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return store.Event{}, fmt.Errorf("delegate: marshal %s payload: %w", typ, err)
	}
	keyID, err := activeKeyID(ctx, tx, sessionID)
	if err != nil {
		return store.Event{}, err
	}
	dek, err := d.Keys.Unwrap(ctx, tx, keyID)
	if err != nil {
		return store.Event{}, fmt.Errorf("delegate: unwrap key for session %s: %w", sessionID, err)
	}
	sealed, err := crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
	if err != nil {
		return store.Event{}, fmt.Errorf("delegate: seal %s payload: %w", typ, err)
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
		return store.Event{}, fmt.Errorf("delegate: append %s: %w", typ, err)
	}
	if d.Chain != nil {
		if _, err := d.Chain.Append(ctx, tx, tenantID, sessionID, out.Seq, out.EventID, string(out.Type), out.PayloadDigest); err != nil {
			return store.Event{}, fmt.Errorf("delegate: chain %s: %w", typ, err)
		}
	}
	return out, nil
}
