package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	cryptopg "github.com/truongpx396/nexus-agent-demo/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces"
)

// Ledger implements surfaces.DeliveryLedger over the deliveries table. Each
// call is one tenant-scoped transaction that also appends the delivery's
// audit event, sealed under the session's own key and hash-chained through
// Chain.
type Ledger struct {
	st    *store.Store
	keys  *cryptopg.KeyStore
	chain *audit.Chain
}

var _ surfaces.DeliveryLedger = (*Ledger)(nil)

// NewLedger returns a Ledger. chain may be nil, in which case events are
// appended without a signed receipt (a minimal wiring for tests).
func NewLedger(st *store.Store, keys *cryptopg.KeyStore, chain *audit.Chain) *Ledger {
	return &Ledger{st: st, keys: keys, chain: chain}
}

// deliveryEnqueuedPayload/deliveryOutcomePayload are what
// EventDeliveryEnqueued/Delivered/Failed carry — deliberately minimal,
// mirroring every other event payload in this codebase (e.g.
// internal/runctl's claimedPayload/claimResolvedPayload): never the
// message body itself, just the audit-relevant facts.
type deliveryEnqueuedPayload struct {
	DeliveryID string `json:"delivery_id"`
	SurfaceID  string `json:"surface_id"`
	Recipient  string `json:"recipient"`
}

type deliveryOutcomePayload struct {
	DeliveryID   string `json:"delivery_id"`
	AttemptCount int    `json:"attempt_count"`
	Reason       string `json:"reason,omitempty"`
}

func (l *Ledger) Open(ctx context.Context, tenantID, sessionID uuid.UUID, seq int64, surfaceID, recipient string) (surfaces.Delivery, error) {
	var d store.Delivery
	err := l.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var created bool
		var err error
		d, created, err = store.OpenOrFindDelivery(ctx, tx, tenantID, sessionID, seq, surfaceID, recipient)
		if err != nil {
			return err
		}
		if created {
			return l.appendEvent(ctx, tx, tenantID, sessionID, eventlog.EventDeliveryEnqueued,
				deliveryEnqueuedPayload{DeliveryID: d.DeliveryID.String(), SurfaceID: surfaceID, Recipient: recipient})
		}
		return nil
	})
	if err != nil {
		return surfaces.Delivery{}, err
	}
	return surfaces.Delivery{ID: d.DeliveryID, Status: surfaces.DeliveryStatus(d.Status), AttemptCount: d.AttemptCount}, nil
}

func (l *Ledger) Record(ctx context.Context, tenantID, sessionID, deliveryID uuid.UUID, status surfaces.DeliveryStatus, reason string) error {
	eventType := eventlog.EventDeliveryFailed
	if status == surfaces.DeliveryDelivered {
		eventType = eventlog.EventDeliveryDelivered
	}
	return l.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		updated, err := store.UpdateDeliveryOutcome(ctx, tx, deliveryID, store.DeliveryStatus(status))
		if err != nil {
			return err
		}
		return l.appendEvent(ctx, tx, tenantID, sessionID, eventType,
			deliveryOutcomePayload{DeliveryID: deliveryID.String(), AttemptCount: updated.AttemptCount, Reason: reason})
	})
}

// activeKeyID and appendEvent are near-identical copies of
// internal/runctl's own same-named functions (internal/runctl/append.go),
// which are themselves a copy of internal/oversight's — see either's own
// doc comment for why duplicating rather than importing is the right call:
// every session in this demo has exactly one DEK for its whole lifetime,
// so "the session's most recent non-erasure key_id" is still the correct,
// and only, notion of "the current key," and this package has no live
// kernel.RunState.Seal closure to reuse instead.
func (l *Ledger) activeKeyID(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (string, error) {
	var keyID string
	err := tx.QueryRow(ctx,
		`SELECT key_id FROM events WHERE session_id = $1 AND key_id != $2 ORDER BY seq DESC LIMIT 1`,
		sessionID, crypto.ErasureKeyID,
	).Scan(&keyID)
	if err != nil {
		return "", fmt.Errorf("surfaces: find active key for session %s: %w", sessionID, err)
	}
	return keyID, nil
}

func (l *Ledger) appendEvent(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, typ eventlog.EventType, payload any) error {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("surfaces: marshal %s payload: %w", typ, err)
	}
	keyID, err := l.activeKeyID(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	dek, err := l.keys.Unwrap(ctx, tx, keyID)
	if err != nil {
		return fmt.Errorf("surfaces: unwrap key for session %s: %w", sessionID, err)
	}
	sealed, err := crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
	if err != nil {
		return fmt.Errorf("surfaces: seal %s payload: %w", typ, err)
	}
	out, err := store.Append(ctx, tx, eventlog.Event{
		EventID: uuid.New(), SessionID: sessionID, TenantID: tenantID, SchemaVersion: eventlog.CurrentSchemaVersion,
		Type: typ, Payload: sealed, PayloadDigest: crypto.Digest(plaintext), KeyID: dek.KeyID, Actor: eventlog.ActorSystem,
	})
	if err != nil {
		return fmt.Errorf("surfaces: append %s: %w", typ, err)
	}
	if l.chain != nil {
		if _, err := l.chain.Append(ctx, tx, tenantID, sessionID, out.Seq, out.EventID, string(out.Type), out.PayloadDigest); err != nil {
			return fmt.Errorf("surfaces: chain %s: %w", typ, err)
		}
	}
	return nil
}
