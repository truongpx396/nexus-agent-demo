package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DeliveryStatus is the deliveries.status column's vocabulary (migrations/
// 0015_deliveries.sql, README task 7.14).
type DeliveryStatus string

const (
	DeliveryPending         DeliveryStatus = "pending"
	DeliveryDelivered       DeliveryStatus = "delivered"
	DeliveryFailed          DeliveryStatus = "failed"
	DeliveryFailedPermanent DeliveryStatus = "failed_permanent"
	DeliverySuppressed      DeliveryStatus = "suppressed"
)

// Delivery mirrors one deliveries row — the outbox's own idempotency
// ledger (README task 7.14, pattern #50), keyed by
// (session_id, seq, surface_id, recipient): a retry of the exact same
// triple finds this SAME row rather than creating a duplicate. Like Claim,
// this carries no notion of the run's own content — the event log's
// EventDeliveryEnqueued/Delivered/Failed events are the audit trail; this
// row is only the durability/idempotency bookkeeping.
type Delivery struct {
	DeliveryID   uuid.UUID
	TenantID     uuid.UUID
	SessionID    uuid.UUID
	Seq          int64
	SurfaceID    string
	Recipient    string
	Status       DeliveryStatus
	AttemptCount int
	CreatedAt    time.Time
	DeliveredAt  *time.Time
}

// OpenOrFindDelivery inserts a fresh pending delivery for
// (sessionID, seq, surfaceID, recipient) if none exists yet (created=true),
// or returns the EXISTING row unchanged (created=false) — the same
// insert-or-fetch-atomically-under-the-UNIQUE-constraint idiom
// OpenOrFindClaim (claims.go) already uses, for the identical reason: a
// caller must never race a SELECT-then-INSERT under concurrent retries.
func OpenOrFindDelivery(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, seq int64, surfaceID, recipient string) (d Delivery, created bool, err error) {
	deliveryID := uuid.New()
	row := tx.QueryRow(ctx, `
		INSERT INTO deliveries (delivery_id, tenant_id, session_id, seq, surface_id, recipient, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (session_id, seq, surface_id, recipient) DO NOTHING
		RETURNING delivery_id, tenant_id, session_id, seq, surface_id, recipient, status, attempt_count, created_at, delivered_at`,
		deliveryID, tenantID, sessionID, seq, surfaceID, recipient, DeliveryPending,
	)
	scanErr := scanDeliveryInto(&d, row)
	if scanErr == nil {
		return d, true, nil
	}
	if !errors.Is(scanErr, pgx.ErrNoRows) {
		return Delivery{}, false, fmt.Errorf("open delivery: %w", scanErr)
	}

	// A real conflict: another row already exists for this
	// (session_id, seq, surface_id, recipient) — read it back unchanged.
	d, err = scanDelivery(tx.QueryRow(ctx, deliverySelectSQL+` WHERE session_id = $1 AND seq = $2 AND surface_id = $3 AND recipient = $4`,
		sessionID, seq, surfaceID, recipient))
	if err != nil {
		return Delivery{}, false, fmt.Errorf("open delivery: read back after conflict: %w", err)
	}
	return d, false, nil
}

// UpdateDeliveryOutcome records one delivery attempt's outcome: increments
// attempt_count and moves status to delivered (terminal, success), failed
// (retryable — a later call can still try again), or failed_permanent
// (terminal, the retry cap was reached). The WHERE clause only ever
// touches a row still in pending/failed — a delivery already
// delivered/failed_permanent/suppressed is terminal and this is a no-op
// against it, which is exactly what makes a repeat Outbox.Deliver call
// against an already-resolved delivery idempotent rather than a duplicate
// send.
func UpdateDeliveryOutcome(ctx context.Context, tx pgx.Tx, deliveryID uuid.UUID, status DeliveryStatus) (Delivery, error) {
	deliveredAtClause := ""
	if status == DeliveryDelivered {
		deliveredAtClause = ", delivered_at = now()"
	}
	_, err := tx.Exec(ctx,
		`UPDATE deliveries SET status = $2, attempt_count = attempt_count + 1`+deliveredAtClause+`
		 WHERE delivery_id = $1 AND status IN ('pending', 'failed')`,
		deliveryID, status,
	)
	if err != nil {
		return Delivery{}, fmt.Errorf("update delivery %s: %w", deliveryID, err)
	}
	d, err := scanDelivery(tx.QueryRow(ctx, deliverySelectSQL+` WHERE delivery_id = $1`, deliveryID))
	if err != nil {
		return Delivery{}, fmt.Errorf("update delivery %s: read back: %w", deliveryID, err)
	}
	return d, nil
}

const deliverySelectSQL = `
	SELECT delivery_id, tenant_id, session_id, seq, surface_id, recipient, status, attempt_count, created_at, delivered_at
	FROM deliveries`

func scanDelivery(r rowScanner) (Delivery, error) {
	var d Delivery
	if err := scanDeliveryInto(&d, r); err != nil {
		return Delivery{}, err
	}
	return d, nil
}

func scanDeliveryInto(d *Delivery, r rowScanner) error {
	err := r.Scan(&d.DeliveryID, &d.TenantID, &d.SessionID, &d.Seq, &d.SurfaceID, &d.Recipient, &d.Status, &d.AttemptCount, &d.CreatedAt, &d.DeliveredAt)
	if err != nil {
		return fmt.Errorf("scan delivery: %w", err)
	}
	return nil
}
