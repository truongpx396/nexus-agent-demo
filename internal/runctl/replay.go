package runctl

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// ReplayResult is what Replay reports: the repaired history and any
// synthetic results Hygiene would backfill (structural, no decrypt), the
// Projection ReplayProjection derives from that same structural pass, and
// how many stored events needed an upcast to reach the current schema
// version — proof the path in store.Upcast is actually exercised against
// this session's real history, not merely present in the registry.
type ReplayResult struct {
	History          []store.Event
	Synthetic        []kernel.SyntheticResult
	Projection       store.Projection
	UpcastedPayloads int
}

// Replay is task 6.10: PURE — no model call, no tool call, no append. It
// answers two questions from a session's own durable log alone: what does
// Hygiene + ReplayProjection say the session's structural shape and status
// are (never decrypting anything for that half), and does every stored
// event's payload still upcast cleanly to the CURRENT schema version (which
// DOES need one decrypt per event, since store.Upcast's registered
// transforms operate on plaintext JSON shapes, never ciphertext — the same
// selective-decrypt layering ReplayFullProjection and kernel.Rehydrate
// already use elsewhere in this codebase).
func (c *Control) Replay(ctx context.Context, tenantID, sessionID uuid.UUID) (ReplayResult, error) {
	var history []store.Event
	if err := c.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		history, err = store.ListEvents(ctx, tx, sessionID)
		return err
	}); err != nil {
		return ReplayResult{}, fmt.Errorf("runctl: replay: %w", err)
	}

	kept, synth := kernel.Hygiene(history)
	projection := store.ReplayProjection(kept)

	decrypt := c.decryptFuncFor(tenantID, sessionID)
	upcasted := 0
	for _, e := range kept {
		plaintext, err := decrypt(ctx, e)
		if err != nil {
			return ReplayResult{}, fmt.Errorf("runctl: replay: decrypt event %s for upcast verification: %w", e.EventID, err)
		}
		_, newVersion, err := store.Upcast(e.Type, e.SchemaVersion, plaintext)
		if err != nil {
			return ReplayResult{}, fmt.Errorf("runctl: replay: verify upcast for event %s (%s): %w", e.EventID, e.Type, err)
		}
		if newVersion != e.SchemaVersion {
			upcasted++
		}
	}

	return ReplayResult{History: kept, Synthetic: synth, Projection: projection, UpcastedPayloads: upcasted}, nil
}
