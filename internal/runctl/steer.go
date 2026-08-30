package runctl

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

type userMessagePayload struct {
	Body string `json:"body"`
}

// Steer is task 6.9's own second named operation: inject a new user message
// into a session out of band. "Steering into a suspended run invalidates
// its approval" (the task's own wording) is implemented exactly as written:
// a suspended session's one pending approval is released
// (oversight.Approvals.Invalidate, InvalidationSteer) with a synthetic
// tool_result before the steer message is appended — the model is told the
// suspended call was superseded, not left dangling.
//
// "Drained at a turn boundary under the serial lock" describes how a LIVE
// worker (internal/queue's Worker, holding this session's SessionLock)
// would apply a queued steer between turns — this demo's synchronous REST
// fast path has no live-goroutine interrupt to drain into (the same honest
// gap internal/tools/pipeline.go's own step-12 doc comment names for
// cross-worker concurrency generally), so Steer here durably records the
// message and returns; the NEXT turn boundary any resumer reaches (a live
// goroutine's own next iteration, or a fresh internal/runctl.Resume call)
// is what actually sees it, because it is already part of the session's
// own transcript by the time either looks.
func (c *Control) Steer(ctx context.Context, tenantID, sessionID uuid.UUID, input string) (store.Event, error) {
	d := c.deps()
	var ev store.Event
	err := c.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		sess, err := store.GetSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if sess.Status == store.SessionStatusCompleted || sess.Status == store.SessionStatusFailed {
			return fmt.Errorf("runctl: session %s is already terminal (%s); cannot steer", sessionID, sess.Status)
		}
		if sess.Status == store.SessionStatusSuspended {
			if _, err := c.Approvals.Invalidate(ctx, tenantID, sessionID, oversight.InvalidationSteer); err != nil {
				return fmt.Errorf("runctl: steer: invalidate pending approval: %w", err)
			}
		}
		ev, err = d.appendEvent(ctx, tx, tenantID, sessionID, store.EventUserMessage, nil, nil, userMessagePayload{Body: input})
		return err
	})
	if err != nil {
		return store.Event{}, err
	}
	return ev, nil
}
