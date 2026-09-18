package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ClaimInboundDelivery is the cheap first line of defense a webhook-facing
// surface (telegram/zalo/email dispatch) takes against a provider's own
// retry behavior, before any session lookup or run is started: an INSERT
// racing migrations/0024_inbound_deliveries.sql's UNIQUE (tenant_id,
// surface_id, delivery_id) constraint is the whole mechanism. The first
// caller to reach this row for a given provider-native delivery id
// (Telegram's update_id, Zalo's msg_id, email's Message-ID) gets
// claimed=true and goes on to dispatch the turn; every later call for the
// SAME triple — a genuine provider redelivery, or two goroutines racing
// the identical delivery across nexusd pods (this table, like every other,
// is the shared source of truth, never per-process memory) — gets
// claimed=false and must treat the delivery as already handled.
//
// This is deliberately simpler than Claim (claims.go, pattern #25,
// resolved by probe or human): a webhook redelivery is an at-least-once
// INBOUND signal a provider controls, not an outbound side effect whose
// own outcome can be ambiguous, so a plain insert-once row is the honest
// mechanism here, not a shortcut. The one accepted trade-off, matching
// this codebase's other "cheap first line of defense" seams: if a claimed
// delivery's own dispatch fails before completing (a crash between the
// claim and the run actually starting), a provider retry of that SAME
// delivery id is now permanently deduped even though it was never really
// processed — the session-key SessionLock (surfaces.AcquireSessionLock)
// and the surface's own conversational continuity are what recover from
// that, not a second automatic delivery of the original input.
func ClaimInboundDelivery(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, surfaceID, deliveryID, sessionKey string) (claimed bool, err error) {
	if deliveryID == "" {
		// Some providers' inbound payloads carry no stable id at all
		// (this codebase's own review: Zalo's minimal event shape and a
		// bare email relay both can arrive this way) — dedup has nothing
		// honest to key on, so the delivery is treated as unclaimed/unique
		// rather than silently colliding unrelated messages against each
		// other on the empty string.
		return true, nil
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO inbound_deliveries (tenant_id, surface_id, delivery_id, session_key)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, surface_id, delivery_id) DO NOTHING`,
		tenantID, surfaceID, deliveryID, sessionKey,
	)
	if err != nil {
		return false, fmt.Errorf("claim inbound delivery %s/%s: %w", surfaceID, deliveryID, err)
	}
	return tag.RowsAffected() == 1, nil
}
