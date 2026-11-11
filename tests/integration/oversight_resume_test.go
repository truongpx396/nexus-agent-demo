//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	oversightpg "github.com/truongpx396/nexus-agent-demo/internal/oversight/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/tools/kerneladapter"
	"github.com/truongpx396/nexus-agent-demo/kernel"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// A decision is recorded on its own and the run is resumed afterwards, by a queue
// worker. The resume has to act on the recorded decision, run the approved call
// once, and be safe to run again: a lease that is reclaimed after a worker dies
// delivers the same job twice.
func TestOversightApproval_ResumeDecidedRunsTheApprovedCallOnceAndIsSafeToRepeat(t *testing.T) {
	rig := setupOversightRig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	workspaceRoot := t.TempDir()

	pipeline, catalog := newFileWritePipeline(t, workspaceRoot)
	approvals := oversight.NewApprovals(oversightpg.New(rig.st, rig.keys, rig.chain))
	prov := fake.New(
		fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "tool_use", ToolUseID: "tu1", ToolName: "platform/file_write@v1", Input: `{"path":"out.txt","content":"approved"}`},
			{Kind: "done", Done: "stop"},
		}},
		fake.Script{Chunks: []fake.ChunkSpec{{Kind: "content", Text: "all done"}, {Kind: "done", Done: "stop"}}},
	)
	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{prov}), Tools: kerneladapter.PipelineExecutor{Pipeline: pipeline},
		Budget: kernel.NoopBudgetGate{},
		Log:    postgres.New(rig.st, postgres.Hooks{Chain: rig.chain, Suspend: rig.onSuspend(approvals)}),
	}

	rig.createSession(t, sessionID, userID, "supervised")
	cfg := kernel.RunConfig{System: "test", Catalog: catalog, ModelID: "fake", MaxTurns: 10, AutonomyLevel: "supervised"}
	runToCompletion(t, k, &kernel.RunState{TenantID: rig.tenantID, SessionID: sessionID, Seal: rig.seal(t, sessionID)}, cfg)

	pending, err := approvals.ListPending(ctx, rig.tenantID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPending: err=%v count=%d", err, len(pending))
	}
	approvalID := pending[0].ApprovalID

	// Only the decision. The run stays suspended until something resumes it.
	if _, err := approvals.Grant(ctx, rig.tenantID, approvalID, userID.String()); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if got := rig.getSession(t, sessionID).Status; got != eventlog.SessionStatusSuspended {
		t.Fatalf("status after the decision = %q, want the run still suspended", got)
	}

	sessions := oversightpg.New(rig.st, rig.keys, rig.chain)
	resumer := &oversight.Resumer{Kernel: k, Approvals: approvals, Sessions: sessions, Keys: sessions, System: "test", Catalog: catalog, MaxTurns: 10}

	var first int
	for _, rerr := range resumer.ResumeDecided(ctx, rig.tenantID, approvalID) {
		if rerr != nil {
			t.Fatalf("ResumeDecided: %v", rerr)
		}
		first++
	}
	final := rig.getSession(t, sessionID)
	if first == 0 || final.Status != eventlog.SessionStatusCompleted {
		t.Fatalf("after the resume: %d events, status %q, want the run completed", first, final.Status)
	}

	// The same job again, as a reclaimed lease would deliver it.
	var second int
	for _, rerr := range resumer.ResumeDecided(ctx, rig.tenantID, approvalID) {
		if rerr != nil {
			t.Fatalf("a repeated ResumeDecided: %v", rerr)
		}
		second++
	}
	if second != 0 {
		t.Fatalf("a repeated resume produced %d events, want none: the call was already answered", second)
	}
	events := listEvents(t, rig, sessionID)
	if got := countEvents(events, eventlog.EventToolResult); got != 2 {
		t.Fatalf("%d tool_result events, want the placeholder and the one real result", got)
	}
}

// An approval that was never decided has nothing to resume, and says so in a way
// a worker can tell from a transient failure.
func TestOversightApproval_ResumeDecidedRefusesAnUndecidedApproval(t *testing.T) {
	rig := setupOversightRig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	pipeline, catalog := newFileWritePipeline(t, t.TempDir())
	approvals := oversight.NewApprovals(oversightpg.New(rig.st, rig.keys, rig.chain))
	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fake.New(fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "tool_use", ToolUseID: "tu1", ToolName: "platform/file_write@v1", Input: `{"path":"x.txt","content":"x"}`},
			{Kind: "done", Done: "stop"},
		}})}),
		Tools: kerneladapter.PipelineExecutor{Pipeline: pipeline}, Budget: kernel.NoopBudgetGate{},
		Log: postgres.New(rig.st, postgres.Hooks{Chain: rig.chain, Suspend: rig.onSuspend(approvals)}),
	}
	rig.createSession(t, sessionID, userID, "supervised")
	runToCompletion(t, k, &kernel.RunState{TenantID: rig.tenantID, SessionID: sessionID, Seal: rig.seal(t, sessionID)},
		kernel.RunConfig{System: "test", Catalog: catalog, ModelID: "fake", MaxTurns: 10, AutonomyLevel: "supervised"})
	pending, _ := approvals.ListPending(ctx, rig.tenantID)

	sessions := oversightpg.New(rig.st, rig.keys, rig.chain)
	resumer := &oversight.Resumer{Kernel: k, Approvals: approvals, Sessions: sessions, Keys: sessions, System: "test", Catalog: catalog, MaxTurns: 10}
	var got error
	for _, rerr := range resumer.ResumeDecided(ctx, rig.tenantID, pending[0].ApprovalID) {
		got = rerr
	}
	if !errors.Is(got, oversight.ErrNotDecided) {
		t.Fatalf("ResumeDecided on a pending approval = %v, want ErrNotDecided", got)
	}
}

// Recovery finds a decision nobody resumed: the run is still suspended long after
// the approval was decided. A decision made a moment ago is still in flight, an
// approval whose run has moved on is not unresumed, and a pending one is not
// decided at all.
func TestOversightApproval_ListUnresumedFindsOnlyADecisionWhoseRunIsStillSuspended(t *testing.T) {
	ctx := context.Background()
	pool := bareAdminPool(t)
	if _, err := store.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	tenant := uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO tenants (tenant_id, name) VALUES ($1, 'unresumed-test')`, tenant)

	newSession := func(status string) uuid.UUID {
		id := uuid.New()
		err := func() error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := store.CreateSession(ctx, tx, eventlog.Session{
				SessionID: id, SessionKey: id.String(), TenantID: tenant, SurfaceID: "test", UserID: uuid.New(),
				AgentVersion: 1, HarnessDigest: []byte("d"), DataLabel: "internal", RouteModelID: "fake", AutonomyLevel: "supervised",
			}); err != nil {
				return err
			}
			if err := store.UpdateSessionStatus(ctx, tx, id, status, nil); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		return id
	}
	approval := func(session uuid.UUID, status string, decidedAgo time.Duration) uuid.UUID {
		id := uuid.New()
		var decidedAt any
		if decidedAgo > 0 {
			decidedAt = time.Now().Add(-decidedAgo)
		}
		exec(`INSERT INTO approvals (approval_id, tenant_id, session_id, tool_use_event_id, tool_id, ask_kind, canonical_digest, context_package, assignee, status, expires_at, decided_at)
		      VALUES ($1, $2, $3, $4, 'platform/file_write@v1', 'once', '\x00', '{}', 'u', $5, now() + interval '1 hour', $6)`,
			id, tenant, session, uuid.New(), status, decidedAt)
		return id
	}

	stuck := approval(newSession(eventlog.SessionStatusSuspended), "granted", 10*time.Minute)
	deniedStuck := approval(newSession(eventlog.SessionStatusSuspended), "denied", 10*time.Minute)
	approval(newSession(eventlog.SessionStatusSuspended), "granted", 5*time.Second)  // decided a moment ago: its job is still in flight
	approval(newSession(eventlog.SessionStatusRunning), "granted", 10*time.Minute)   // already resumed
	approval(newSession(eventlog.SessionStatusCompleted), "granted", 10*time.Minute) // finished
	approval(newSession(eventlog.SessionStatusSuspended), "pending", 0)              // not decided
	approval(newSession(eventlog.SessionStatusSuspended), "invalidated", 10*time.Minute)

	got, err := oversightpg.ListUnresumed(ctx, pool)
	if err != nil {
		t.Fatalf("ListUnresumed: %v", err)
	}
	found := map[uuid.UUID]bool{}
	for _, u := range got {
		found[u.ApprovalID] = true
		if u.TenantID != tenant {
			t.Errorf("unresumed %v names tenant %v, want %v", u.ApprovalID, u.TenantID, tenant)
		}
	}
	if len(got) != 2 || !found[stuck] || !found[deniedStuck] {
		t.Fatalf("found %v, want exactly the granted and the denied approval whose runs are still suspended", got)
	}
}

func countEvents(events []eventlog.Event, typ eventlog.EventType) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func listEvents(t *testing.T, rig *oversightRig, sessionID uuid.UUID) []eventlog.Event {
	t.Helper()
	var events []eventlog.Event
	err := rig.st.InTenantTx(context.Background(), rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var lerr error
		events, lerr = store.ListEvents(ctx, tx, sessionID)
		return lerr
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return events
}
