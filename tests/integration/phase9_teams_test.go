//go:build integration

// Phase 9 — Peer agent teams: shared task boards (README.md §9). Covers the
// two named acceptance criteria: 9.4 (no card is ever double-claimed, under
// a real contention test against Postgres SKIP LOCKED) and 9.6 (a clean
// reader is provably tainted by reading a tainted card, and a flagged card
// never launders anything). Also covers the roster/envelope/completion
// round trip (9.1, 9.8, 9.9) and the leaf bound (9.10).
//
// Shares oversightRig/setupPostgresAndPgBouncer/insertTenant/
// listEventsDirect/hasEventType/contentScript with phase5_oversight_test.go
// and phase8_orchestration_test.go (same package).
package integration

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/teams"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// buildTeamsRig wires a real *teams.Service against the shared oversightRig
// (Postgres-backed), plus a *runctl.Control as its Canceler — everything
// cmd/nexusd wires in production for teamsSvc, minus the REST/CLI surfaces
// this test never needs.
func buildTeamsRig(t *testing.T, r *oversightRig, prov provider.Provider) (*teams.Service, *kernel.Kernel) {
	t.Helper()
	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{prov}),
		Tools:    kernel.NotImplementedToolExecutor{}, // this file's own member scripts never call a tool
		Store:    r.st,
		Receipts: r.receiptFunc(),
	}
	approvals := oversight.NewApprovals(r.st, r.keys, r.chain)
	ctl := &runctl.Control{Store: r.st, Keys: r.keys, Chain: r.chain, Approvals: approvals, Kernel: k, System: "test", MaxTurns: 10}

	svc := teams.NewService(r.st, r.keys, r.chain)
	svc.Wire(teams.Config{Kernel: k, Canceler: ctl, System: "test", MaxTurns: 10})
	return svc, k
}

func ceiling(micros int64) cost.Money {
	return cost.Money{Micros: micros, Currency: cost.DefaultCurrency}
}

// mustCreateTeamRow and mustCreateCardRow insert directly rather than going
// through Service.CreateTeam/WriteCard — the pragmatic, honest fixture
// choice tests/integration/phase8_orchestration_test.go's own
// mustCreatePendingDelegation already makes for the analogous "I only need
// the ROW, not the live machinery around it" case.
func mustCreateTeamRow(t *testing.T, r *oversightRig, teamID, coordinatorSessionID uuid.UUID) {
	t.Helper()
	err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO teams (team_id, tenant_id, name, coordinator_session_id, roster)
			VALUES ($1,$2,$3,$4,'[]'::jsonb)`,
			teamID, r.tenantID, "test-team", coordinatorSessionID,
		)
		return err
	})
	if err != nil {
		t.Fatalf("insert team row: %v", err)
	}
}

func mustCreateCardRow(t *testing.T, r *oversightRig, teamID, cardID, writtenBy uuid.UUID) {
	t.Helper()
	err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO board_cards (card_id, tenant_id, team_id, title, body, injection_scan_status, written_by_session_id)
			VALUES ($1,$2,$3,'card','body','clean',$4)`,
			cardID, r.tenantID, teamID, writtenBy,
		)
		return err
	})
	if err != nil {
		t.Fatalf("insert card row: %v", err)
	}
}

// mustBootstrapSessionKey gives sessionID an active encryption key by
// sealing one throwaway EventContent directly (bypassing kernel.Run
// entirely) — internal/teams' own appendEvent (events.go) resolves a
// session's ACTIVE key from its most recent event, the same convention
// internal/delegate and internal/runctl already follow for every
// out-of-band append; a session this test never drove through a real
// kernel.Run (r.createSession alone leaves it with zero events) has no such
// key yet. A real coordinator/member session always has one by the time
// any board tool or CreateTeam ever touches it, because dispatch only
// happens mid- or post- a live turn that already sealed at least one event
// — this helper reproduces exactly that precondition, nothing more.
func mustBootstrapSessionKey(t *testing.T, r *oversightRig, sessionID uuid.UUID) {
	t.Helper()
	seal := r.seal(t, sessionID)
	sealed, digest, keyID, err := seal([]byte(`{"body":"bootstrap"}`))
	if err != nil {
		t.Fatalf("seal bootstrap event: %v", err)
	}
	err = r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.Append(ctx, tx, store.Event{
			EventID: uuid.New(), SessionID: sessionID, TenantID: r.tenantID,
			SchemaVersion: store.CurrentSchemaVersion, Type: store.EventContent,
			Payload: sealed, PayloadDigest: digest, KeyID: keyID, Actor: store.ActorSystem,
		})
		return err
	})
	if err != nil {
		t.Fatalf("append bootstrap event: %v", err)
	}
}

// TestClaimCard_ContentionRace_ExactlyOneWinner is README task 9.4's own
// acceptance test: N sessions race to claim the SAME card_id concurrently —
// exactly one must win, proven by the real SKIP LOCKED claim query against
// Postgres, never a double claim.
func TestClaimCard_ContentionRace_ExactlyOneWinner(t *testing.T) {
	r := setupOversightRig(t)
	svc, _ := buildTeamsRig(t, r, fake.New())

	coordinatorID, userID := uuid.New(), uuid.New()
	r.createSession(t, coordinatorID, userID, "autonomous")
	teamID := uuid.New()
	mustCreateTeamRow(t, r, teamID, coordinatorID)
	cardID := uuid.New()
	mustCreateCardRow(t, r, teamID, cardID, coordinatorID)

	const n = 8
	memberIDs := make([]uuid.UUID, n)
	for i := range memberIDs {
		memberIDs[i] = uuid.New()
		r.createSession(t, memberIDs[i], userID, "autonomous")
		mustBootstrapSessionKey(t, r, memberIDs[i]) // ClaimCard's own read-time fold appends onto the claimant
	}

	var wg sync.WaitGroup
	results := make([]bool, n)
	var errs [n]error
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, claimed, err := svc.ClaimCard(context.Background(), r.tenantID, teamID, cardID, memberIDs[i])
			results[i], errs[i] = claimed, err
		}(i)
	}
	wg.Wait()

	var wins int
	for i, ok := range results {
		if errs[i] != nil {
			t.Fatalf("ClaimCard[%d]: %v", i, errs[i])
		}
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("got %d winners racing for the same card, want exactly 1", wins)
	}

	status := mustGetCardStatus(t, r, cardID)
	if status != "claimed" {
		t.Fatalf("card status = %q, want claimed", status)
	}
}

// mustGetCardStatus is a direct-query fixture helper — internal/teams
// exposes no "get one card" method (ReadBoard lists a whole board), and a
// direct query here is the pragmatic choice, same as mustCreateCardRow
// above.
func mustGetCardStatus(t *testing.T, r *oversightRig, cardID uuid.UUID) string {
	t.Helper()
	var status string
	err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM board_cards WHERE card_id = $1`, cardID).Scan(&status)
	})
	if err != nil {
		t.Fatalf("get card %s status: %v", cardID, err)
	}
	return status
}

// TestReadBoard_FoldsTaintFromCleanCard_NeverFromFlagged is README task
// 9.6's own acceptance test — the read-time analogue of task 8.11's
// return-time fold test: a card written under a tainted session is read by
// a clean member, whose own taint_state picks up the untrusted leg; a
// flagged card (task 9.7) is never surfaced and never folds anything.
func TestReadBoard_FoldsTaintFromCleanCard_NeverFromFlagged(t *testing.T) {
	r := setupOversightRig(t)
	svc, _ := buildTeamsRig(t, r, fake.New())

	writerID, readerID, userID := uuid.New(), uuid.New(), uuid.New()
	r.createSession(t, writerID, userID, "autonomous")
	r.createSession(t, readerID, userID, "autonomous")
	mustBootstrapSessionKey(t, r, readerID) // ReadBoard's own read-time fold appends onto the reader
	coordinatorID := writerID
	teamID := uuid.New()
	mustCreateTeamRow(t, r, teamID, coordinatorID)

	// No Pipeline is wired on this rig (buildTeamsRig's own teams.Config
	// leaves Pipeline nil), so TaintStateFor(writerID) reads as the zero
	// value — insert the WRITTEN card directly with an explicit
	// taint_state instead, the same "I only need the row" fixture choice
	// mustCreateCardRow already makes, so this test can assert the FOLD
	// (ReadBoard's own job) independent of the CAPTURE (WriteCard's job,
	// already covered at the unit level by internal/tools/builtin's own
	// fakes).
	cleanCardID := uuid.New()
	if err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO board_cards (card_id, tenant_id, team_id, title, body, taint_state, injection_scan_status, written_by_session_id)
			VALUES ($1,$2,$3,'clean card','clean body','[true,false,false]'::jsonb,'clean',$4)`,
			cleanCardID, r.tenantID, teamID, writerID,
		)
		return err
	}); err != nil {
		t.Fatalf("insert clean card: %v", err)
	}

	flaggedCardID := uuid.New()
	secret := "sk-" + strings.Repeat("a", 25) // memory.Screen's own exfiltration pattern
	if err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO board_cards (card_id, tenant_id, team_id, title, body, taint_state, injection_scan_status, written_by_session_id)
			VALUES ($1,$2,$3,'flagged card',$4,'[false,true,false]'::jsonb,'flagged',$5)`,
			flaggedCardID, r.tenantID, teamID, secret, writerID,
		)
		return err
	}); err != nil {
		t.Fatalf("insert flagged card: %v", err)
	}

	cards, err := svc.ReadBoard(context.Background(), r.tenantID, teamID, readerID)
	if err != nil {
		t.Fatalf("ReadBoard: %v", err)
	}
	if len(cards) != 2 {
		t.Fatalf("got %d cards, want 2", len(cards))
	}
	for _, c := range cards {
		if c.CardID == flaggedCardID && c.Body != "" {
			t.Fatalf("flagged card's body was surfaced to the reader: %q (README task 9.7)", c.Body)
		}
	}

	events, err := listEventsDirect(context.Background(), r.st, r.tenantID, readerID)
	if err != nil {
		t.Fatalf("list reader events: %v", err)
	}
	if !hasEventType(events, store.EventTaintTransition) {
		t.Fatalf("reader's own log never shows a taint_transition — the clean card's read never folded")
	}

	var transitions int
	for _, e := range events {
		if e.Type == store.EventTaintTransition {
			transitions++
		}
	}
	if transitions != 1 {
		t.Fatalf("reader's log carries %d taint_transition events, want exactly 1 (only the clean card should ever fold — the flagged one must not)", transitions)
	}
}

// TestCreateTeam_SpawnsMembersAndCompletesWhenBoardEmptyAndMembersTerminal
// is the roster/envelope/completion round trip (README tasks 9.1, 9.2, 9.8,
// 9.9): CreateTeam reserves one shared envelope, spawns every roster
// member as an ordinary session, and the team transitions to 'completed'
// once every member reaches a terminal state on an empty board.
func TestCreateTeam_SpawnsMembersAndCompletesWhenBoardEmptyAndMembersTerminal(t *testing.T) {
	r := setupOversightRig(t)
	prov := fake.New(contentScript("member 1 done"), contentScript("member 2 done"))
	svc, _ := buildTeamsRig(t, r, prov)

	coordinatorID, userID := uuid.New(), uuid.New()
	r.createSession(t, coordinatorID, userID, "autonomous")
	mustBootstrapSessionKey(t, r, coordinatorID) // CreateTeam appends team_created/team_ended onto the coordinator

	teamID, err := svc.CreateTeam(context.Background(), teams.CreateTeamRequest{
		TenantID: r.tenantID, CreatorSessionID: coordinatorID, Name: "demo-team",
		Members: []teams.MemberSpec{{AgentID: "worker-1", Task: "go"}, {AgentID: "worker-2", Task: "go"}},
		Ceiling: ceiling(1_000_000),
	})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var team teams.Team
	for time.Now().Before(deadline) {
		team, err = svc.Get(context.Background(), r.tenantID, teamID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if team.Status == teams.StatusCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if team.Status != teams.StatusCompleted {
		t.Fatalf("team status = %q, want completed (last reason: %q)", team.Status, team.Reason)
	}

	var memberCount int
	if err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE team_id = $1 AND delegation_role = 'team_member'`, teamID).Scan(&memberCount)
	}); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if memberCount != 2 {
		t.Fatalf("got %d member sessions, want 2", memberCount)
	}

	coordinatorEvents, err := listEventsDirect(context.Background(), r.st, r.tenantID, coordinatorID)
	if err != nil {
		t.Fatalf("list coordinator events: %v", err)
	}
	if !hasEventType(coordinatorEvents, store.EventTeamCreated) {
		t.Fatalf("coordinator's own log never shows team_created")
	}
	if !hasEventType(coordinatorEvents, store.EventTeamEnded) {
		t.Fatalf("coordinator's own log never shows team_ended")
	}
}

// TestCreateTeam_DeniesNestedTeamCreation is README task 9.10's own bound:
// a session already at the depth bound (the same MaxDepth a delegate's own
// child is refused at, task 8.12) may not create a team either — no
// recursive teams, no depth workaround through a side door.
func TestCreateTeam_DeniesNestedTeamCreation(t *testing.T) {
	r := setupOversightRig(t)
	svc, _ := buildTeamsRig(t, r, fake.New())

	rootID, memberID, userID := uuid.New(), uuid.New(), uuid.New()
	r.createSession(t, rootID, userID, "autonomous")
	// A session already at teams.MaxDepth (1) — the same depth a team
	// member itself would be created at.
	if err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.CreateSession(ctx, tx, store.Session{
			SessionID: memberID, SessionKey: memberID.String(), TenantID: r.tenantID,
			SurfaceID: "test", UserID: userID, AgentID: uuid.Nil, AgentVersion: 1,
			HarnessDigest: []byte("test"), DataLabel: "internal", RouteModelID: "fake",
			AutonomyLevel: "autonomous", RootSessionID: rootID, Depth: teams.MaxDepth, DelegationRole: "team_member",
		})
	}); err != nil {
		t.Fatalf("create depth-bound session: %v", err)
	}

	_, err := svc.CreateTeam(context.Background(), teams.CreateTeamRequest{
		TenantID: r.tenantID, CreatorSessionID: memberID, Name: "nested",
		Members: []teams.MemberSpec{{AgentID: "worker", Task: "go"}},
		Ceiling: ceiling(1_000_000),
	})
	if err == nil {
		t.Fatalf("CreateTeam succeeded from a session already at the depth bound, want refusal")
	}
}
