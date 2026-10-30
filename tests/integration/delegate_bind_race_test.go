//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// TestDelegate_ChildFinishingBeforeItsDelegationIsBound pins an ordering the
// round-trip test above only hits by luck. Spawn starts the child on its own
// goroutine and returns; the parent then appends delegation_requested and
// binds the delegation to that tool_use in a transaction of its own. A child
// that finishes first used to resolve against a delegation with no tool_use to
// pair its result with, after which the parent's Bind found nothing pending and
// the parent's run errored. A scripted child finishes in milliseconds, so the
// bookkeeping is delayed here to make the child win every time; with a real
// model the window is narrower but it is the same window.
func TestDelegate_ChildFinishingBeforeItsDelegationIsBound(t *testing.T) {
	r := setupOversightRig(t)
	prov := fake.New(
		delegateToolUse("worker", "summarize the notes", "platform/delegate@v1"),
		contentScript("child summary: all good"),
		contentScript("parent: done, thanks"),
	)
	k, _, d, catalog := buildDelegationRig(t, r, prov, nil)

	const bindDelay = 750 * time.Millisecond
	k.Log = postgres.New(r.st, postgres.Hooks{
		Receipt: r.receiptFunc(),
		Delegate: func(ctx context.Context, tx pgx.Tx, req kernel.DelegateSuspendRequest) error {
			time.Sleep(bindDelay)
			return onDelegateFunc(d)(ctx, tx, req)
		},
	})

	parentID, userID := uuid.New(), uuid.New()
	r.createSession(t, parentID, userID, "autonomous")
	st := &kernel.RunState{TenantID: r.tenantID, SessionID: parentID, Seal: r.seal(t, parentID)}
	cfg := kernel.RunConfig{System: "test", Catalog: catalog, MaxTurns: 10, AutonomyLevel: "autonomous", Input: "please delegate this"}

	for _, err := range k.Run(context.Background(), st, cfg) {
		if err != nil {
			t.Fatalf("the parent's run failed while its child had already finished: %v", err)
		}
	}

	final := waitForStatus(t, r, parentID, eventlog.SessionStatusCompleted)
	if final.TerminalReason == nil || *final.TerminalReason != "completed" {
		t.Fatalf("parent terminal reason = %v, want completed", final.TerminalReason)
	}
	events, err := listEventsDirect(context.Background(), r.st, r.tenantID, parentID)
	if err != nil {
		t.Fatalf("list parent events: %v", err)
	}
	if !hasEventType(events, eventlog.EventDelegationReturned) {
		t.Error("parent's log never shows delegation_returned")
	}
	delegations := mustListDelegations(t, r, parentID)
	if len(delegations) != 1 {
		t.Fatalf("%d delegations for the parent, want 1", len(delegations))
	}
	if delegations[0].Status != delegate.StatusReturned {
		t.Errorf("delegation status = %q, want returned", delegations[0].Status)
	}
	// mustListDelegations reads the status column only, so ask for the binding itself.
	var bound bool
	if err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT parent_tool_use_event_id IS NOT NULL FROM delegations WHERE parent_session_id = $1`, parentID).Scan(&bound)
	}); err != nil {
		t.Fatalf("read the binding: %v", err)
	}
	if !bound {
		t.Error("the delegation was resolved without ever being bound to its tool_use")
	}
}
