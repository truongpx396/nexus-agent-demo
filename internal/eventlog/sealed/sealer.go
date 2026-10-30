// Package sealed is the Postgres implementation of eventlog.Appender: events
// sealed under a session's own key and chained, in the caller's transaction.
// It is deliberately not part of internal/eventlog/postgres, which implements
// kernel.EventLog and so imports the kernel; the surfaces' adapters need this
// without that.
package sealed

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditpg "github.com/truongpx396/nexus-agent-demo/internal/audit/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	cryptopg "github.com/truongpx396/nexus-agent-demo/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Sealer builds eventlog.Appenders over Postgres. It holds the collaborators
// every out-of-band service needs and used to carry its own copy of: the
// tenant's key store (to seal under the session's own DEK, the same way every
// other event in the session is sealed) and the audit chain (nil is valid — no
// receipt is written, matching the nil convention of every optional control).
type Sealer struct {
	keys  *cryptopg.KeyStore
	chain *auditpg.Chain
}

// NewSealer returns a Sealer.
func NewSealer(keys *cryptopg.KeyStore, chain *auditpg.Chain) *Sealer {
	return &Sealer{keys: keys, chain: chain}
}

// Bind returns an Appender whose every Append runs in tx.
func (s *Sealer) Bind(tx pgx.Tx) eventlog.Appender { return sealedAppender{s: s, tx: tx} }

type sealedAppender struct {
	s  *Sealer
	tx pgx.Tx
}

var _ eventlog.Appender = sealedAppender{}

// activeKeyID finds the key_id a session's events are currently sealed
// under — its most recent event whose KeyID isn't crypto.ErasureKeyID (an
// erasure record is deliberately plaintext, never a real key). Every session
// in this demo has exactly one DEK for its whole lifetime (one is minted per
// session and never rotated), so this is equivalent to "the session's DEK".
func activeKeyID(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (string, error) {
	var keyID string
	err := tx.QueryRow(ctx,
		`SELECT key_id FROM events WHERE session_id = $1 AND key_id != $2 ORDER BY seq DESC LIMIT 1`,
		sessionID, crypto.ErasureKeyID,
	).Scan(&keyID)
	if err != nil {
		return "", fmt.Errorf("eventlog: find active key for session %s: %w", sessionID, err)
	}
	return keyID, nil
}

// Append is the same marshal -> seal -> append -> chain sequence
// kernel/loop.go's own appendEvent runs, for callers that operate out of
// band, after a live kernel.Run/Resume call has already returned and with no
// RunState.Seal closure to reuse.
func (a sealedAppender) Append(ctx context.Context, tenantID, sessionID uuid.UUID, typ eventlog.EventType, toolID *string, pairRef *uuid.UUID, payload any) (eventlog.Event, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return eventlog.Event{}, fmt.Errorf("eventlog: marshal %s payload: %w", typ, err)
	}
	keyID, err := activeKeyID(ctx, a.tx, sessionID)
	if err != nil {
		return eventlog.Event{}, err
	}
	dek, err := a.s.keys.Unwrap(ctx, a.tx, keyID)
	if err != nil {
		return eventlog.Event{}, fmt.Errorf("eventlog: unwrap key for session %s: %w", sessionID, err)
	}
	sealed, err := crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
	if err != nil {
		return eventlog.Event{}, fmt.Errorf("eventlog: seal %s payload: %w", typ, err)
	}
	return a.appendSealed(ctx, tenantID, sessionID, eventlog.Event{
		EventID: uuid.New(), SessionID: sessionID, TenantID: tenantID, SchemaVersion: eventlog.CurrentSchemaVersion,
		Type: typ, Payload: sealed, PayloadDigest: crypto.Digest(plaintext), KeyID: dek.KeyID,
		Actor: eventlog.ActorSystem, ToolID: toolID, PairRef: pairRef,
	})
}

// appendSealed appends an already-sealed event and chains its receipt — the tail
// every append shares, whichever way its payload was sealed.
func (a sealedAppender) appendSealed(ctx context.Context, tenantID, sessionID uuid.UUID, e eventlog.Event) (eventlog.Event, error) {
	out, err := store.Append(ctx, a.tx, e)
	if err != nil {
		return eventlog.Event{}, fmt.Errorf("eventlog: append %s: %w", e.Type, err)
	}
	if a.s.chain != nil {
		if _, err := a.s.chain.Append(ctx, a.tx, tenantID, sessionID, out.Seq, out.EventID, string(out.Type), out.PayloadDigest); err != nil {
			return eventlog.Event{}, fmt.Errorf("eventlog: chain %s: %w", e.Type, err)
		}
	}
	return out, nil
}
