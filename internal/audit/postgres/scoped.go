package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent/internal/audit"
	"github.com/truongpx396/nexus-agent/internal/store"
)

// Scoped is a Chain bound to a Store, for the callers that are not already
// inside a transaction: the scheduled anchor-and-verify pass and the control
// plane's receipt backend. Inside a transaction (the event log's append, a
// service's unit of work) the Chain is used directly, so the receipt commits
// with the event it covers.
type Scoped struct {
	St    *store.Store
	Chain *Chain
}

// EmitReceipt extends the chain by one receipt in its own tenant-scoped
// transaction.
func (s Scoped) EmitReceipt(ctx context.Context, tenantID, sessionID uuid.UUID, seq int64, eventID uuid.UUID, eventType string, payloadDigest []byte) error {
	return s.St.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.Chain.Append(ctx, tx, tenantID, sessionID, seq, eventID, eventType, payloadDigest)
		return err
	})
}

// AnchorAndVerify anchors tenantID's new receipts and then verifies the whole
// chain (README task 5.3's scheduled verifier). A break or a gap is logged, not
// returned: it is an alert for whatever pages the operator, not a reason to fail
// the pass that is the source of truth for the thing it is checking. The error
// is for a pass that could not run at all.
func (s Scoped) AnchorAndVerify(ctx context.Context, tenantID uuid.UUID) error {
	return s.St.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, _, err := s.Chain.Anchor(ctx, tx, tenantID); err != nil {
			return err
		}
		report, err := s.Chain.Verify(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		logIfBroken(tenantID, report)
		return nil
	})
}

func logIfBroken(tenantID uuid.UUID, report audit.Report) {
	if !report.OK() {
		log.Error().Any("tenant_id", tenantID).Any("breaks", report.Breaks).Any("gaps", report.Gaps).Msg("audit: chain verification found a problem")
	}
}
