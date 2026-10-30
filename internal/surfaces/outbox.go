// Package surfaces holds the primitives every concrete surface shares
// (README task 7.14, pattern #50): the delivery outbox and inbound dedup.
// Concrete surfaces live one package down (internal/surfaces/rest,
// internal/surfaces/cli, and Phase 11's telegram/zalo/email/cron/mcp) so this
// package itself stays free of any one surface's own transport details —
// Outbox.Deliver takes a Sender interface, never a concrete transport, and
// reaches storage only through DeliveryLedger. The Postgres adapter for both
// ports is internal/surfaces/postgres.
package surfaces

import (
	"context"
	"fmt"

	"github.com/google/uuid"
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

// DeliveryStatus is where one delivery stands (migrations/0015_deliveries.sql).
type DeliveryStatus string

const (
	DeliveryPending         DeliveryStatus = "pending"          // opened, never attempted
	DeliveryDelivered       DeliveryStatus = "delivered"        // terminal
	DeliveryFailed          DeliveryStatus = "failed"           // attempted, retryable
	DeliveryFailedPermanent DeliveryStatus = "failed_permanent" // terminal: the retry cap was reached
	DeliverySuppressed      DeliveryStatus = "suppressed"       // terminal
)

// Delivery is one row of the outbox's idempotency ledger.
type Delivery struct {
	ID           uuid.UUID
	Status       DeliveryStatus
	AttemptCount int // attempts already recorded
}

// DeliveryLedger is the outbox's durable side. Each method is one unit of
// work in the tenant's own scope, and each writes the audit event that
// describes it in the same unit, hash-chained by the adapter: a delivery
// state and its event never disagree.
type DeliveryLedger interface {
	// Open finds or creates the idempotency row for (sessionID, seq,
	// surfaceID, recipient). Only when it creates the row does it append
	// delivery_enqueued, so a retry of the same triple finds the same row and
	// audits nothing new.
	Open(ctx context.Context, tenantID, sessionID uuid.UUID, seq int64, surfaceID, recipient string) (Delivery, error)

	// Record applies one attempt's outcome to the delivery and appends the
	// matching event: delivery_delivered when status is DeliveryDelivered,
	// delivery_failed otherwise (reason says why).
	Record(ctx context.Context, tenantID, sessionID, deliveryID uuid.UUID, status DeliveryStatus, reason string) error
}

// Outbox is the durable, at-least-once delivery ledger (task 7.14):
// EventDeliveryEnqueued is appended BEFORE Sender.Send is ever attempted;
// idempotent on (session_id, seq, surface_id, recipient) via
// migrations/0015_deliveries.sql's UNIQUE constraint — a retry of the
// exact same triple finds the SAME row, never a duplicate send once that
// row is terminal. failed_permanent (after maxDeliveryAttempts) stays
// distinguishable from a delivery nobody has attempted yet (pending).
type Outbox struct {
	Ledger DeliveryLedger
}

// Deliver makes ONE delivery attempt for (sessionID, seq, surfaceID,
// recipient): opens (or finds) the idempotency row, and — only if it isn't
// already terminal (delivered/failed_permanent/suppressed) — calls
// sender.Send exactly once and records the outcome. A caller wanting
// at-least-once delivery in the face of transient failure calls this
// again (its own retry loop, with backoff) for the SAME key; Deliver
// itself never loops or sleeps.
func (o *Outbox) Deliver(ctx context.Context, tenantID, sessionID uuid.UUID, seq int64, surfaceID, recipient string, payload []byte, sender Sender) error {
	delivery, err := o.Ledger.Open(ctx, tenantID, sessionID, seq, surfaceID, recipient)
	if err != nil {
		return fmt.Errorf("surfaces: open delivery: %w", err)
	}

	switch delivery.Status { //nolint:exhaustive // deliberately narrow: pending and failed are the only two statuses this method ever acts on below; delivered/failed_permanent/suppressed all fall through to the terminal no-op return, which is exactly the idempotency this method's own doc comment promises
	case DeliveryDelivered, DeliveryFailedPermanent, DeliverySuppressed:
		return nil // terminal: a repeat call against an already-resolved delivery is a no-op, never a duplicate send
	}

	sendErr := sender.Send(ctx, surfaceID, recipient, payload)

	status := DeliveryDelivered
	reason := ""
	if sendErr != nil {
		reason = sendErr.Error()
		status = DeliveryFailed
		if delivery.AttemptCount+1 >= maxDeliveryAttempts {
			status = DeliveryFailedPermanent
		}
	}

	if err := o.Ledger.Record(ctx, tenantID, sessionID, delivery.ID, status, reason); err != nil {
		return fmt.Errorf("surfaces: record delivery outcome: %w", err)
	}
	return nil
}
