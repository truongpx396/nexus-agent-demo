package teams_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/teams"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

var tenant = uuid.New()

// fakeCanceler records the sessions it was asked to cancel, and what the store
// held at that moment, so a test can pin the order endTeam works in.
type fakeCanceler struct {
	cancelled []uuid.UUID
	onCancel  func()
}

func (c *fakeCanceler) Cancel(_ context.Context, _, sessionID uuid.UUID, _ string) error {
	c.cancelled = append(c.cancelled, sessionID)
	if c.onCancel != nil {
		c.onCancel()
	}
	return nil
}

func newService(f *fakeStore) (*teams.Service, *fakeCanceler) {
	c := &fakeCanceler{}
	return teams.NewService(f).Wire(teams.Config{Canceler: c}), c
}

// finish moves a member to a terminal status the way a finished run does.
func finish(f *fakeStore, s eventlog.Session, status string, reason *string) eventlog.Session {
	s.Status, s.TerminalReason = status, reason
	f.sessions[s.SessionID] = s
	return s
}

func TestOnMemberTerminal_IsANoOpWhereNothingEnds(t *testing.T) {
	t.Run("a session that is no team member", func(t *testing.T) {
		f := newFakeStore()
		svc, _ := newService(f)
		plain := eventlog.Session{SessionID: uuid.New(), Status: eventlog.SessionStatusCompleted}
		f.sessions[plain.SessionID] = plain
		if err := svc.OnMemberTerminal(context.Background(), tenant, plain.SessionID); err != nil || len(f.events) != 0 {
			t.Errorf("OnMemberTerminal = %v, events %v, want a no-op", err, f.eventTypes())
		}
	})
	t.Run("a member that has not finished", func(t *testing.T) {
		f := newFakeStore()
		svc, _ := newService(f)
		team, members := f.seedTeam(1, eventlog.SessionStatusRunning)
		if err := svc.OnMemberTerminal(context.Background(), tenant, members[0].SessionID); err != nil {
			t.Fatalf("OnMemberTerminal: %v", err)
		}
		if f.teams[team.TeamID].Status != teams.StatusActive || len(f.events) != 0 {
			t.Errorf("team = %s, events %v, want it still active", f.teams[team.TeamID].Status, f.eventTypes())
		}
	})
}

func TestOnMemberTerminal_CompletesTheTeamWhenTheBoardIsEmptyAndEveryoneIsDone(t *testing.T) {
	f := newFakeStore()
	svc, _ := newService(f)
	team, members := f.seedTeam(2, eventlog.SessionStatusRunning)
	f.seedCard(team.TeamID, teams.CardDone, teams.ScanClean)

	finish(f, members[0], eventlog.SessionStatusCompleted, nil)
	if err := svc.OnMemberTerminal(context.Background(), tenant, members[0].SessionID); err != nil {
		t.Fatalf("OnMemberTerminal: %v", err)
	}
	if f.teams[team.TeamID].Status != teams.StatusActive {
		t.Fatal("the team ended while one member was still running")
	}

	finish(f, members[1], eventlog.SessionStatusCompleted, nil)
	if err := svc.OnMemberTerminal(context.Background(), tenant, members[1].SessionID); err != nil {
		t.Fatalf("OnMemberTerminal: %v", err)
	}
	if got := f.teams[team.TeamID]; got.Status != teams.StatusCompleted {
		t.Errorf("team = %s, want completed", got.Status)
	}
	if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventTeamEnded {
		t.Errorf("events = %v, want exactly one team_ended", types)
	}
}

func TestOnMemberTerminal_LeavesATeamActiveWhileCardsRemainOpen(t *testing.T) {
	f := newFakeStore()
	svc, _ := newService(f)
	team, members := f.seedTeam(1, eventlog.SessionStatusRunning)
	f.seedCard(team.TeamID, teams.CardOpen, teams.ScanClean)
	finish(f, members[0], eventlog.SessionStatusCompleted, nil)

	if err := svc.OnMemberTerminal(context.Background(), tenant, members[0].SessionID); err != nil {
		t.Fatalf("OnMemberTerminal: %v", err)
	}
	if f.teams[team.TeamID].Status != teams.StatusActive {
		t.Errorf("team = %s, want it left active for the backstop: guessing it is done would drop open work", f.teams[team.TeamID].Status)
	}
}

func TestOnMemberTerminal_AMemberThatExhaustedTheEnvelopeEndsTheTeamAtOnce(t *testing.T) {
	f := newFakeStore()
	svc, canceler := newService(f)
	team, members := f.seedTeam(2, eventlog.SessionStatusRunning)
	f.seedCard(team.TeamID, teams.CardOpen, teams.ScanClean) // work remains, yet the budget is gone
	reason := string(kernel.ReasonCostExhausted)
	finish(f, members[0], eventlog.SessionStatusFailed, &reason)

	if err := svc.OnMemberTerminal(context.Background(), tenant, members[0].SessionID); err != nil {
		t.Fatalf("OnMemberTerminal: %v", err)
	}
	if got := f.teams[team.TeamID]; got.Status != teams.StatusCeilingExhausted {
		t.Errorf("team = %s, want ceiling_exhausted", got.Status)
	}
	if len(canceler.cancelled) != 1 || canceler.cancelled[0] != members[1].SessionID {
		t.Errorf("cancelled %v, want only the member still running (%s)", canceler.cancelled, members[1].SessionID)
	}
}

func TestSweepBackstop_AbortsAStaleTeamAndReapsItsMembers(t *testing.T) {
	f := newFakeStore()
	svc, canceler := newService(f)
	team, members := f.seedTeam(1, eventlog.SessionStatusRunning)
	team.CreatedAt = time.Now().Add(-2 * time.Hour)
	f.teams[team.TeamID] = team
	fresh, _ := f.seedTeam(0, "")
	fresh.CreatedAt = time.Now()
	f.teams[fresh.TeamID] = fresh

	if err := svc.SweepBackstop(context.Background(), tenant, time.Hour); err != nil {
		t.Fatalf("SweepBackstop: %v", err)
	}
	if f.teams[team.TeamID].Status != teams.StatusAborted || f.teams[fresh.TeamID].Status != teams.StatusActive {
		t.Errorf("stale = %s, fresh = %s, want only the stale team aborted", f.teams[team.TeamID].Status, f.teams[fresh.TeamID].Status)
	}
	if len(canceler.cancelled) != 1 || canceler.cancelled[0] != members[0].SessionID {
		t.Errorf("cancelled %v, want the stale team's running member", canceler.cancelled)
	}
}

// The regression test for the durability gap: ending a team used to commit the
// status change, then reap, then append team_ended in a third commit, so a crash
// (or an event that could not be recorded) between them left a team marked ended
// with no team_ended event, and nothing could repair it because the claim only
// succeeds from active. The status and the event are now one unit.
func TestEndTeam_StatusAndEventCommitTogether(t *testing.T) {
	f := newFakeStore()
	svc, _ := newService(f)
	team, members := f.seedTeam(1, eventlog.SessionStatusRunning)
	team.CreatedAt = time.Now().Add(-2 * time.Hour)
	f.teams[team.TeamID] = team
	_ = members

	f.failAppend = errors.New("audit chain unavailable")
	if err := svc.SweepBackstop(context.Background(), tenant, time.Hour); err != nil {
		t.Fatalf("SweepBackstop: %v", err) // it logs a failed team and carries on
	}
	if got := f.teams[team.TeamID].Status; got != teams.StatusActive {
		t.Fatalf("team = %s after team_ended could not be recorded, want it still active: a completed team with no event can never be repaired", got)
	}
	if len(f.events) != 0 {
		t.Fatalf("events = %v, want none from the failed attempt", f.eventTypes())
	}

	// The event can be recorded again, so the next pass ends the team, once.
	f.failAppend = nil
	if err := svc.SweepBackstop(context.Background(), tenant, time.Hour); err != nil {
		t.Fatalf("SweepBackstop retry: %v", err)
	}
	if got := f.teams[team.TeamID].Status; got != teams.StatusAborted {
		t.Errorf("team = %s after the retry, want aborted", got)
	}
	if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventTeamEnded {
		t.Errorf("events = %v, want exactly one team_ended on the coordinator", types)
	}
	if f.events[0].SessionID != team.CoordinatorSessionID {
		t.Errorf("team_ended landed on %s, want the coordinator %s", f.events[0].SessionID, team.CoordinatorSessionID)
	}
}

func TestEndTeam_ReapsMembersBeforeTheTeamIsMarkedEnded(t *testing.T) {
	f := newFakeStore()
	svc, canceler := newService(f)
	team, _ := f.seedTeam(2, eventlog.SessionStatusRunning)
	team.CreatedAt = time.Now().Add(-2 * time.Hour)
	f.teams[team.TeamID] = team
	canceler.onCancel = func() {
		// Members are cancelled while the team is still active, so a crash
		// part-way leaves a team the backstop will end, not an ended team with
		// running members.
		if f.teams[team.TeamID].Status != teams.StatusActive {
			t.Error("a member was cancelled after the team had already been marked ended")
		}
	}

	if err := svc.SweepBackstop(context.Background(), tenant, time.Hour); err != nil {
		t.Fatalf("SweepBackstop: %v", err)
	}
	if len(canceler.cancelled) != 2 {
		t.Errorf("cancelled %d members, want both", len(canceler.cancelled))
	}
}

func TestEndTeam_EndingAnAlreadyEndedTeamRecordsNothingTwice(t *testing.T) {
	f := newFakeStore()
	svc, _ := newService(f)
	team, members := f.seedTeam(1, eventlog.SessionStatusRunning)
	reason := string(kernel.ReasonCostExhausted)
	finish(f, members[0], eventlog.SessionStatusFailed, &reason)

	for i := 0; i < 2; i++ {
		if err := svc.OnMemberTerminal(context.Background(), tenant, members[0].SessionID); err != nil {
			t.Fatalf("OnMemberTerminal #%d: %v", i+1, err)
		}
	}
	if types := f.eventTypes(); len(types) != 1 {
		t.Errorf("events = %v after two identical end requests, want one team_ended: the second caller loses the claim", types)
	}
	if f.teams[team.TeamID].Status != teams.StatusCeilingExhausted {
		t.Errorf("team = %s, want the first status kept", f.teams[team.TeamID].Status)
	}
}

func TestCreateTeam_RefusesWhatItMustBeforeSpawningAnything(t *testing.T) {
	f := newFakeStore()
	svc := teams.NewService(f).Wire(teams.Config{Kernel: &kernel.Kernel{}})

	if _, err := svc.CreateTeam(context.Background(), teams.CreateTeamRequest{TenantID: tenant, CreatorSessionID: uuid.New()}); err == nil {
		t.Error("a team with an empty roster was created")
	}

	// A session already at the depth bound — including any team member, which is
	// created one level down — cannot create a team (task 9.10).
	deep := eventlog.Session{SessionID: uuid.New(), Depth: teams.MaxDepth}
	f.sessions[deep.SessionID] = deep
	_, err := svc.CreateTeam(context.Background(), teams.CreateTeamRequest{
		TenantID: tenant, CreatorSessionID: deep.SessionID, Members: []teams.MemberSpec{{AgentID: "a", Task: "t"}},
		Ceiling: cost.Money{Micros: 1000, Currency: "USD"},
	})
	if err == nil {
		t.Fatal("a session at the depth bound created a team")
	}
	if len(f.teams) != 0 || len(f.events) != 0 {
		t.Errorf("a refused creation left %d teams and events %v behind", len(f.teams), f.eventTypes())
	}

	if _, err := teams.NewService(f).CreateTeam(context.Background(), teams.CreateTeamRequest{TenantID: tenant}); err == nil {
		t.Error("CreateTeam succeeded before Wire")
	}
}

func TestEnvelopeBudgetGate_MembersShareOneCeilingAndAreRefusedOnceItIsSpent(t *testing.T) {
	f := newFakeStore()
	id := uuid.New()
	f.envelopes[id] = &envelope{ceiling: cost.Money{Micros: 2000, Currency: "USD"}, remaining: cost.Money{Micros: 2000, Currency: "USD"}, members: 2}
	g := &teams.EnvelopeBudgetGate{Store: f, EnvelopeID: id, TenantID: tenant, PerCallEstimate: cost.Money{Micros: 1000, Currency: "USD"}, Currency: "USD"}

	for i := 0; i < 2; i++ {
		if res, err := g.Reserve(context.Background(), cost.ReserveRequest{TenantID: tenant, SessionID: uuid.New()}); err != nil || res.Decision.Kind != cost.DecisionAllow {
			t.Fatalf("Reserve %d = %v, %v, want allowed while the shared envelope has room", i+1, res.Decision.Kind, err)
		}
	}
	res, err := g.Reserve(context.Background(), cost.ReserveRequest{TenantID: tenant, SessionID: uuid.New()})
	if err == nil || res.Decision.Kind != cost.DecisionRefuseCeiling {
		t.Errorf("third Reserve = %v, %v, want refused: no member draws on a ceiling of its own", res.Decision.Kind, err)
	}

	missing := &teams.EnvelopeBudgetGate{Store: f, EnvelopeID: uuid.New(), TenantID: tenant, PerCallEstimate: cost.Money{Micros: 1, Currency: "USD"}}
	if res, err := missing.Reserve(context.Background(), cost.ReserveRequest{TenantID: tenant, SessionID: uuid.New()}); err == nil || res.Decision.Kind != cost.DecisionRefuseCeiling {
		t.Errorf("Reserve against an envelope that does not exist = %v, %v, want refused (fail closed)", res.Decision.Kind, err)
	}
}
