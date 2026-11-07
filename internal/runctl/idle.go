package runctl

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// SweepIdle ends every conversation in one tenant that has sat awaiting input
// since before olderThan, with nobody sending the next message — the
// idle-conversation backstop. Each session is ended on its own through
// EndIdleConversation, so one failure is logged and the sweep carries on; only
// failing to list the stale sessions is returned. The caller owns the clock and
// the window (policy), and decides which tenants to sweep.
func (c *Control) SweepIdle(ctx context.Context, tenantID uuid.UUID, olderThan time.Time) error {
	var stale []uuid.UUID
	if err := c.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		stale, err = tx.ListStaleAwaitingInput(ctx, olderThan)
		return err
	}); err != nil {
		return fmt.Errorf("runctl: list stale awaiting-input sessions: %w", err)
	}
	for _, sessionID := range stale {
		if err := c.EndIdleConversation(ctx, tenantID, sessionID); err != nil {
			log.Error().Err(err).Any("tenant_id", tenantID).Any("session_id", sessionID).Msg("runctl: idle-conversation sweep: end session failed")
		}
	}
	return nil
}
