package runctl

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/queue"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// ResumeConversation continues a conversational session (store.Session.
// Conversational, migrations/0023_conversational_sessions.sql) that
// kernel.Kernel.suspendForUserInput paused in store.SessionStatusAwaitingInput
// — the human's next message. It now mirrors Steer's own shape exactly
// (steer.go): durably append the message and flip the session back to
// running, out of band, then let a worker pool actually drive the turn
// loop via a queued queue.KindConverse job — rather than doing so on this
// call's own goroutine, which is what internal/surfaces/rest's
// handleSteerRun used to do with a same-process `go s.publishUntilDone(...)`.
// That was the identical single-process scalability gap a fresh run's own
// Kernel.Seed + queue.KindStart split closes (cmd/nexusd's
// kernelRunStarter.StartRun) — this closes it here too, and for the same
// reason: nothing about the message that was JUST appended needs to be
// carried through the queue job itself, so the job carries only ids, never
// this method's own plaintext input.
func (c *Control) ResumeConversation(ctx context.Context, tenantID, sessionID uuid.UUID, input string) (store.Event, error) {
	d := c.deps()
	var ev store.Event
	err := c.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		sess, err := store.GetSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if sess.Status != store.SessionStatusAwaitingInput {
			return fmt.Errorf("runctl: session %s is %q, not awaiting_input; nothing to resume a conversation into", sessionID, sess.Status)
		}
		ev, err = d.appendEvent(ctx, tx, tenantID, sessionID, store.EventUserMessage, nil, nil, userMessagePayload{Body: input})
		if err != nil {
			return err
		}
		return store.UpdateSessionStatus(ctx, tx, sessionID, store.SessionStatusRunning, nil)
	})
	if err != nil {
		return store.Event{}, err
	}

	if c.Queue != nil {
		if _, err := c.Queue.Enqueue(ctx, queue.Job{
			TenantID: tenantID, SessionID: sessionID, SessionKey: sessionID.String(), Kind: queue.KindConverse,
		}); err != nil {
			return store.Event{}, fmt.Errorf("runctl: resume conversation: enqueue continue job: %w", err)
		}
	}
	return ev, nil
}

// EndIdleConversation durably ends ONE session that has sat in
// store.SessionStatusAwaitingInput past cmd/nexusd's idle-conversation sweep
// window with nobody sending the next message — the same shape Cancel
// already uses for an explicit human cancel (append an EventTerminal, mark
// the session store.SessionStatusFailed), just with kernel.TerminalIdleTimeout
// as the reason instead of TerminalAborted, so a golden-signal query can
// tell "the user stopped this" apart from "nobody came back." A session no
// longer awaiting_input (already resumed, or ended by a concurrent sweep
// pass/cancel) is a no-op, not an error — matching Cancel's own
// already-terminal no-op convention.
func (c *Control) EndIdleConversation(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	d := c.deps()
	return c.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		sess, err := store.GetSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if sess.Status != store.SessionStatusAwaitingInput {
			return nil
		}

		t := kernel.TerminalIdleTimeout("no reply received before the idle-conversation sweep window elapsed")
		payload := terminalPayload{Reason: string(t.Reason), Detail: t.Detail}
		if _, err := d.appendEvent(ctx, tx, tenantID, sessionID, store.EventTerminal, nil, nil, payload); err != nil {
			return fmt.Errorf("end idle conversation: append terminal event: %w", err)
		}
		terminalReason := string(t.Reason)
		if err := store.UpdateSessionStatus(ctx, tx, sessionID, store.SessionStatusFailed, &terminalReason); err != nil {
			return fmt.Errorf("end idle conversation: update session status: %w", err)
		}
		return nil
	})
}
