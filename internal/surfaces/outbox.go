// Package surfaces holds the one primitive every concrete surface shares
// (README task 7.14, pattern #50): the delivery outbox. Concrete surfaces
// live one package down (internal/surfaces/rest, internal/surfaces/cli,
// and Phase 11's telegram/zalo/email/cron/mcp) so this package itself stays
// free of any one surface's own transport details — Outbox.Deliver takes a
// Sender interface, never a concrete transport.
//
// This package may import kernel-adjacent packages (store/crypto/audit)
// freely — tests/contract/boundaries_test.go's "surfaces must not import
// the kernel" rule is about kernel itself, not its neighbors, and Outbox
// needs the same store-append + audit-chain-append machinery
// internal/runctl and internal/oversight already duplicate locally for the
// identical reason their own doc comments give: each package operates out
// of band, with no live kernel.RunState.Seal closure to reuse.
package surfaces

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

// maxDeliveryAttempts is the fixed retry cap before a delivery is marked
// failed_permanent — small and constant rather than configurable, matching
// this codebase's other fixed-cap seams (e.g. internal/reliability's
// circuit breaker at 3 identical failures).
const maxDeliveryAttempts = 3

// Sender actually delivers one payload to one recipient over one surface —
// implemented per transport (Phase 11's Telegram bot API client, an email
// provider's send call, ...). Outbox owns only the durability/idempotency
// discipline around calling it, never the transport itself.
type Sender interface {
	Send(ctx context.Context, surfaceID, recipient string, payload []byte) error
}

// deliveryEnqueuedPayload/deliveryOutcomePayload are what
// EventDeliveryEnqueued/Delivered/Failed/Suppressed carry — deliberately
// minimal, mirroring every other event payload in this codebase (e.g.
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

// Outbox is the durable, at-least-once delivery ledger (task 7.14):
// EventDeliveryEnqueued is appended BEFORE Sender.Send is ever attempted;
// idempotent on (session_id, seq, surface_id, recipient) via
// migrations/0015_deliveries.sql's UNIQUE constraint — a retry of the
// exact same triple finds the SAME row, never a duplicate send once that
// row is terminal. failed_permanent (after maxDeliveryAttempts) stays
// distinguishable from a delivery nobody has attempted yet (pending).
type Outbox struct {
	Store *store.Store
	Keys  *crypto.KeyStore
	Chain *audit.Chain
}

// Deliver makes ONE delivery attempt for (sessionID, seq, surfaceID,
// recipient): opens (or finds) the idempotency row, and — only if it isn't
// already terminal (delivered/failed_permanent/suppressed) — calls
// sender.Send exactly once and records the outcome. A caller wanting
// at-least-once delivery in the face of transient failure calls this
// again (its own retry loop, with backoff) for the SAME key; Deliver
// itself never loops or sleeps.
func (o *Outbox) Deliver(ctx context.Context, tenantID, sessionID uuid.UUID, seq int64, surfaceID, recipient string, payload []byte, sender Sender) error {
	var delivery store.Delivery
	var created bool
	err := o.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		delivery, created, err = store.OpenOrFindDelivery(ctx, tx, tenantID, sessionID, seq, surfaceID, recipient)
		if err != nil {
			return err
		}
		if created {
			_, err = o.appendEvent(ctx, tx, tenantID, sessionID, store.EventDeliveryEnqueued, nil, nil,
				deliveryEnqueuedPayload{DeliveryID: delivery.DeliveryID.String(), SurfaceID: surfaceID, Recipient: recipient})
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("surfaces: open delivery: %w", err)
	}

	switch delivery.Status { //nolint:exhaustive // deliberately narrow: pending and failed are the only two statuses this method ever acts on below; delivered/failed_permanent/suppressed all fall through to the terminal no-op return, which is exactly the idempotency this method's own doc comment promises
	case store.DeliveryDelivered, store.DeliveryFailedPermanent, store.DeliverySuppressed:
		return nil // terminal: a repeat call against an already-resolved delivery is a no-op, never a duplicate send
	}

	sendErr := sender.Send(ctx, surfaceID, recipient, payload)

	status := store.DeliveryDelivered
	reason := ""
	if sendErr != nil {
		reason = sendErr.Error()
		status = store.DeliveryFailed
		if delivery.AttemptCount+1 >= maxDeliveryAttempts {
			status = store.DeliveryFailedPermanent
		}
	}

	eventType := store.EventDeliveryDelivered
	if sendErr != nil {
		eventType = store.EventDeliveryFailed
	}

	err = o.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		updated, err := store.UpdateDeliveryOutcome(ctx, tx, delivery.DeliveryID, status)
		if err != nil {
			return err
		}
		_, err = o.appendEvent(ctx, tx, tenantID, sessionID, eventType, nil, nil,
			deliveryOutcomePayload{DeliveryID: delivery.DeliveryID.String(), AttemptCount: updated.AttemptCount, Reason: reason})
		return err
	})
	if err != nil {
		return fmt.Errorf("surfaces: record delivery outcome: %w", err)
	}
	return nil
}

// activeKeyID and appendEvent are near-identical copies of
// internal/runctl's own same-named functions (internal/runctl/append.go),
// which are themselves a copy of internal/oversight's — see either's own
// doc comment for why duplicating rather than importing is the right call:
// every session in this demo has exactly one DEK for its whole lifetime,
// so "the session's most recent non-erasure key_id" is still the correct,
// and only, notion of "the current key," and this package has no live
// kernel.RunState.Seal closure to reuse instead.
func (o *Outbox) activeKeyID(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (string, error) {
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

func (o *Outbox) appendEvent(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, typ store.EventType, toolID *string, pairRef *uuid.UUID, payload any) (store.Event, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return store.Event{}, fmt.Errorf("surfaces: marshal %s payload: %w", typ, err)
	}
	keyID, err := o.activeKeyID(ctx, tx, sessionID)
	if err != nil {
		return store.Event{}, err
	}
	dek, err := o.Keys.Unwrap(ctx, tx, keyID)
	if err != nil {
		return store.Event{}, fmt.Errorf("surfaces: unwrap key for session %s: %w", sessionID, err)
	}
	sealed, err := crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
	if err != nil {
		return store.Event{}, fmt.Errorf("surfaces: seal %s payload: %w", typ, err)
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
		return store.Event{}, fmt.Errorf("surfaces: append %s: %w", typ, err)
	}
	if o.Chain != nil {
		if _, err := o.Chain.Append(ctx, tx, tenantID, sessionID, out.Seq, out.EventID, string(out.Type), out.PayloadDigest); err != nil {
			return store.Event{}, fmt.Errorf("surfaces: chain %s: %w", typ, err)
		}
	}
	return out, nil
}
