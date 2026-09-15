package runctl

import (
	"context"
	"fmt"
	"iter"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// ResumeConversation continues a conversational session (store.Session.
// Conversational, migrations/0023_conversational_sessions.sql) that
// kernel.Kernel.suspendForUserInput paused in store.SessionStatusAwaitingInput
// — the human's next message. Unlike Resume (the general crash/steer
// resume, re-entering the loop as-is via kernel.Kernel.Continue) this is
// scoped narrowly to the one state a conversational pause can be in,
// mirroring internal/oversight.Resumer's own "decide, THEN resume" shape:
// refuse if the session isn't actually awaiting input (nothing to resume
// into), rehydrate, then drive kernel.Kernel.ResumeConversation with the
// new message.
func (c *Control) ResumeConversation(ctx context.Context, tenantID, sessionID uuid.UUID, input string) (iter.Seq2[store.Event, error], error) {
	st, sess, err := c.loadRunState(ctx, tenantID, sessionID)
	if err != nil {
		return nil, err
	}
	if sess.Status != store.SessionStatusAwaitingInput {
		return nil, fmt.Errorf("runctl: session %s is %q, not awaiting_input; nothing to resume a conversation into", sessionID, sess.Status)
	}

	cfg := kernel.RunConfig{
		System: c.System, Catalog: c.Catalog, MaxTurns: c.MaxTurns,
		AutonomyLevel: sess.AutonomyLevel, ModelID: sess.RouteModelID,
		Conversational: sess.Conversational,
	}
	return c.Kernel.ResumeConversation(ctx, st, cfg, input), nil
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
