package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditpg "github.com/truongpx396/nexus-agent-demo/internal/audit/postgres"
	cryptopg "github.com/truongpx396/nexus-agent-demo/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/sealed"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces"
)

// Ledger implements surfaces.DeliveryLedger over the deliveries table. Each
// call is one tenant-scoped transaction that also appends the delivery's
// audit event, sealed under the session's own key and hash-chained through
// Chain.
type Ledger struct {
	st     *store.Store
	sealer *sealed.Sealer
}

var _ surfaces.DeliveryLedger = (*Ledger)(nil)

// NewLedger returns a Ledger. chain may be nil, in which case events are
// appended without a signed receipt (a minimal wiring for tests).
func NewLedger(st *store.Store, keys *cryptopg.KeyStore, chain *auditpg.Chain) *Ledger {
	return &Ledger{st: st, sealer: sealed.NewSealer(keys, chain)}
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

// appendEvent appends one delivery audit event, sealed under the session's own
// key and chained, in the caller's transaction.
func (l *Ledger) appendEvent(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, typ eventlog.EventType, payload any) error {
	_, err := l.sealer.Bind(tx).Append(ctx, tenantID, sessionID, typ, nil, nil, payload)
	return err
}
