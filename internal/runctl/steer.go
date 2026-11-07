package runctl

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
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
func (c *Control) Steer(ctx context.Context, tenantID, sessionID uuid.UUID, input string) (eventlog.Event, error) {
	var ev eventlog.Event
	err := c.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		sess, err := tx.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		if sess.Status == eventlog.SessionStatusCompleted || sess.Status == eventlog.SessionStatusFailed {
			return fmt.Errorf("runctl: session %s is already terminal (%s); cannot steer", sessionID, sess.Status)
		}
		if sess.Status == eventlog.SessionStatusAwaitingInput {
			// Steer durably records the message for whatever resumer
			// reaches the next turn boundary (this func's own doc comment)
			// — but a conversational session paused in awaiting_input has
			// no live goroutine to reach one; only ResumeConversation
			// re-enters the loop from here. Refuse rather than silently
			// stranding the message.
			return fmt.Errorf("runctl: session %s is awaiting the next conversation turn; call ResumeConversation with the new message rather than Steer", sessionID)
		}
		if sess.Status == eventlog.SessionStatusSuspended {
			if _, err := c.Approvals.Invalidate(ctx, tenantID, sessionID, oversight.InvalidationSteer); err != nil {
				return fmt.Errorf("runctl: steer: invalidate pending approval: %w", err)
			}
		}
		ev, err = tx.Append(ctx, tenantID, sessionID, eventlog.EventUserMessage, nil, nil, userMessagePayload{Body: input})
		return err
	})
	if err != nil {
		return eventlog.Event{}, err
	}
	return ev, nil
}

// Send delivers a human's message to a session and leaves it to the
// appropriate path to act on it: a conversational session paused in
// awaiting_input is continued (ResumeConversation, which appends the message
// and queues the continuation), and anything else is steered (Steer, which
// nudges a task that is already under way). Steer alone has nothing to steer in
// a paused conversation and refuses it, so a caller that does not know which
// state the session is in, as the REST steer endpoint does not, asks Send.
//
// The status is read first and acted on after, in separate transactions, as the
// REST handler this replaces did. If the session moves between the two, the
// path chosen refuses it (ResumeConversation claims the session atomically,
// Steer rejects a paused one), so the message is never appended twice or into
// the wrong state.
func (c *Control) Send(ctx context.Context, tenantID, sessionID uuid.UUID, input string) error {
	var status string
	if err := c.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		sess, err := tx.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		status = sess.Status
		return nil
	}); err != nil {
		return err
	}
	if status == eventlog.SessionStatusAwaitingInput {
		_, err := c.ResumeConversation(ctx, tenantID, sessionID, input)
		return err
	}
	_, err := c.Steer(ctx, tenantID, sessionID, input)
	return err
}
