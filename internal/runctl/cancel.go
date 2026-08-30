package runctl

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

type terminalPayload struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Cancel is task 6.9's own line: "the sole producer of aborted." It is a
// no-op-safe operation over a session in any non-terminal status: pending
// (queued or running with nothing yet suspended) or suspended.
//
//   - Suspended: release the ONE pending approval with a synthetic result
//     (oversight.Approvals.Invalidate, InvalidationCancel) and every pending
//     input request (oversight.Inputs.Invalidate) before terminating —
//     exactly the sequencing kernel/loop.go's own Resume already documents
//     for a denial: release the paired result, THEN decide the run is over.
//   - Running: this demo has no live-goroutine interrupt wired for the
//     synchronous REST fast path (internal/tools/pipeline.go's own step-12
//     doc comment names the analogous gap for cross-worker concurrency) —
//     Cancel still marks the session aborted here; a live turn loop that
//     is mid-Provider.Stream when this runs will still append its OWN
//     next event through the ordinary store.Append path and simply find
//     the session already terminal on its NEXT status write, which
//     internal/store's own append-only discipline never contradicts (the
//     event still lands; only the session's aborted STATUS was set early).
//
// A session already terminal is a no-op, not an error — matching every
// other "second call is harmless" convention in this codebase.
func (c *Control) Cancel(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) error {
	d := c.deps()
	return c.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		sess, err := store.GetSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if sess.Status == store.SessionStatusCompleted || sess.Status == store.SessionStatusFailed {
			return nil // already terminal — cancel is a no-op, not an error
		}

		if sess.Status == store.SessionStatusSuspended {
			if _, err := c.Approvals.Invalidate(ctx, tenantID, sessionID, oversight.InvalidationCancel); err != nil {
				return fmt.Errorf("cancel: invalidate pending approval: %w", err)
			}
			if c.Inputs != nil {
				if _, err := c.Inputs.Invalidate(ctx, tenantID, sessionID, oversight.InvalidationCancel); err != nil {
					return fmt.Errorf("cancel: invalidate pending input requests: %w", err)
				}
			}
		}

		t := kernel.TerminalAborted(reason)
		payload := terminalPayload{Reason: string(t.Reason), Detail: t.Detail}
		if _, err := d.appendEvent(ctx, tx, tenantID, sessionID, store.EventTerminal, nil, nil, payload); err != nil {
			return fmt.Errorf("cancel: append terminal event: %w", err)
		}
		terminalReason := string(t.Reason)
		if err := store.UpdateSessionStatus(ctx, tx, sessionID, store.SessionStatusFailed, &terminalReason); err != nil {
			return fmt.Errorf("cancel: update session status: %w", err)
		}
		return nil
	})
}
