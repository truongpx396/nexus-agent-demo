package plan

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// deps mirrors internal/oversight and internal/delegate's own unexported
// `deps` type field-for-field — the same out-of-band appendEvent idiom,
// reimplemented here for the same reason each of those gives: Executor
// drives events onto a session with no live RunState.Seal to reuse.
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
		return "", fmt.Errorf("plan: find active key for session %s: %w", sessionID, err)
	}
	return keyID, nil
}

func (d deps) appendEvent(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, typ store.EventType, toolID *string, pairRef *uuid.UUID, payload any) (store.Event, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return store.Event{}, fmt.Errorf("plan: marshal %s payload: %w", typ, err)
	}
	keyID, err := activeKeyID(ctx, tx, sessionID)
	if err != nil {
		return store.Event{}, err
	}
	dek, err := d.Keys.Unwrap(ctx, tx, keyID)
	if err != nil {
		return store.Event{}, fmt.Errorf("plan: unwrap key for session %s: %w", sessionID, err)
	}
	sealed, err := crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
	if err != nil {
		return store.Event{}, fmt.Errorf("plan: seal %s payload: %w", typ, err)
	}
	e := store.Event{
		EventID: uuid.New(), SessionID: sessionID, TenantID: tenantID,
		SchemaVersion: store.CurrentSchemaVersion, Type: typ,
		Payload: sealed, PayloadDigest: crypto.Digest(plaintext), KeyID: dek.KeyID,
		Actor: store.ActorSystem, ToolID: toolID, PairRef: pairRef,
	}
	out, err := store.Append(ctx, tx, e)
	if err != nil {
		return store.Event{}, fmt.Errorf("plan: append %s: %w", typ, err)
	}
	if d.Chain != nil {
		if _, err := d.Chain.Append(ctx, tx, tenantID, sessionID, out.Seq, out.EventID, string(out.Type), out.PayloadDigest); err != nil {
			return store.Event{}, fmt.Errorf("plan: chain %s: %w", typ, err)
		}
	}
	return out, nil
}

// appendFirstEvent seals payload with seal directly (never via
// activeKeyID's "look at the most recent event" lookup, which has nothing
// to find on a session's very first append) and durably appends it —
// Executor.Start's own doc comment explains why this is the one call site
// in this package that can't use the ordinary appendEvent.
func (d deps) appendFirstEvent(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, seal kernel.SealFunc, typ store.EventType, payload any) (store.Event, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return store.Event{}, fmt.Errorf("plan: marshal %s payload: %w", typ, err)
	}
	sealed, digest, keyID, err := seal(plaintext)
	if err != nil {
		return store.Event{}, fmt.Errorf("plan: seal %s payload: %w", typ, err)
	}
	e := store.Event{
		EventID: uuid.New(), SessionID: sessionID, TenantID: tenantID,
		SchemaVersion: store.CurrentSchemaVersion, Type: typ,
		Payload: sealed, PayloadDigest: digest, KeyID: keyID, Actor: store.ActorSystem,
	}
	out, err := store.Append(ctx, tx, e)
	if err != nil {
		return store.Event{}, fmt.Errorf("plan: append %s: %w", typ, err)
	}
	if d.Chain != nil {
		if _, err := d.Chain.Append(ctx, tx, tenantID, sessionID, out.Seq, out.EventID, string(out.Type), out.PayloadDigest); err != nil {
			return store.Event{}, fmt.Errorf("plan: chain %s: %w", typ, err)
		}
	}
	return out, nil
}

// decryptWith returns a decrypt closure over a fresh per-call DEK cache —
// mirrors internal/oversight.Resumer.loadRunState's own inline closure,
// factored out here since both Executor.Start and Executor.Resume need it.
func (d deps) decryptWith(tenantID, sessionID uuid.UUID) func(ctx context.Context, e store.Event) ([]byte, error) {
	cache := map[string]crypto.DEK{}
	return func(ctx context.Context, e store.Event) ([]byte, error) {
		if e.KeyID == crypto.ErasureKeyID {
			return e.Payload, nil
		}
		if dek, ok := cache[e.KeyID]; ok {
			return crypto.Open(dek, e.Payload, tenantID.String(), sessionID.String())
		}
		var dek crypto.DEK
		if err := d.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var uerr error
			dek, uerr = d.Keys.Unwrap(ctx, tx, e.KeyID)
			return uerr
		}); err != nil {
			return nil, err
		}
		cache[e.KeyID] = dek
		return crypto.Open(dek, e.Payload, tenantID.String(), sessionID.String())
	}
}
