package runctl_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

var tenant = uuid.New()

func newControl() (*runctl.Control, *fakeStore, *fakeQueue) {
	f, q := newFakeStore(), &fakeQueue{}
	return &runctl.Control{Store: f, Queue: q}, f, q
}

func TestCancel_EndsARunningSessionWithAnAbortedTerminalEvent(t *testing.T) {
	c, f, _ := newControl()
	s := f.seedSession(eventlog.SessionStatusRunning, "supervised")

	if err := c.Cancel(context.Background(), tenant, s.SessionID, "user pressed stop"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got := f.sessions[s.SessionID]
	if got.Status != eventlog.SessionStatusFailed || got.TerminalReason == nil || *got.TerminalReason != "aborted" {
		t.Errorf("session = %s reason %v, want failed with reason aborted", got.Status, got.TerminalReason)
	}
	if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventTerminal {
		t.Errorf("events = %v, want exactly one terminal event: the status never changes without it", types)
	}
}

func TestCancel_OfAnAlreadyEndedSessionIsAHarmlessNoOp(t *testing.T) {
	for _, status := range []string{eventlog.SessionStatusCompleted, eventlog.SessionStatusFailed} {
		c, f, _ := newControl()
		s := f.seedSession(status, "supervised")
		if err := c.Cancel(context.Background(), tenant, s.SessionID, "again"); err != nil {
			t.Fatalf("Cancel of a %s session: %v", status, err)
		}
		if len(f.events) != 0 || f.sessions[s.SessionID].Status != status {
			t.Errorf("a %s session was changed by a second cancel: events %v", status, f.eventTypes())
		}
	}
}

func TestCancel_IsAllOrNothing(t *testing.T) {
	c, f, _ := newControl()
	s := f.seedSession(eventlog.SessionStatusRunning, "supervised")
	f.failAppend = errors.New("audit chain unavailable")

	if err := c.Cancel(context.Background(), tenant, s.SessionID, "stop"); err == nil {
		t.Fatal("Cancel succeeded though its terminal event could not be recorded")
	}
	if f.sessions[s.SessionID].Status != eventlog.SessionStatusRunning {
		t.Errorf("status = %s after a failed cancel, want it still running: a status must never change without its event", f.sessions[s.SessionID].Status)
	}
}

func TestSteer_AppendsTheMessageToALiveSession(t *testing.T) {
	c, f, _ := newControl()
	s := f.seedSession(eventlog.SessionStatusRunning, "supervised")

	ev, err := c.Steer(context.Background(), tenant, s.SessionID, "use the staging bucket")
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if ev.Type != eventlog.EventUserMessage || len(f.events) != 1 {
		t.Errorf("steer recorded %v, want one user_message", f.eventTypes())
	}
}

func TestSteer_IsRefusedWhereItCouldNotBeHonored(t *testing.T) {
	cases := map[string]string{
		"an ended session":                         eventlog.SessionStatusCompleted,
		"a failed session":                         eventlog.SessionStatusFailed,
		"a conversation waiting for its next turn": eventlog.SessionStatusAwaitingInput,
	}
	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			c, f, _ := newControl()
			s := f.seedSession(status, "supervised")
			if _, err := c.Steer(context.Background(), tenant, s.SessionID, "hello"); err == nil {
				t.Fatal("Steer succeeded: the message would be stranded with no one to read it")
			}
			if len(f.events) != 0 {
				t.Errorf("%d events recorded for a refused steer, want none", len(f.events))
			}
		})
	}
}

func TestTightenAutonomy_MovesTheLevelAndRecordsItTogether(t *testing.T) {
	c, f, _ := newControl()
	s := f.seedSession(eventlog.SessionStatusRunning, "autonomous")

	if err := c.TightenAutonomy(context.Background(), tenant, s.SessionID, "read_only"); err != nil {
		t.Fatalf("TightenAutonomy: %v", err)
	}
	if got := f.sessions[s.SessionID].AutonomyLevel; got != "read_only" {
		t.Errorf("level = %s, want read_only", got)
	}
	if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventAutonomyTightened {
		t.Errorf("events = %v, want autonomy_tightened: never a bare column write with no justifying event", types)
	}
}

func TestTightenAutonomy_NeverWidens(t *testing.T) {
	c, f, _ := newControl()
	s := f.seedSession(eventlog.SessionStatusRunning, "read_only")

	if err := c.TightenAutonomy(context.Background(), tenant, s.SessionID, "autonomous"); err == nil {
		t.Fatal("TightenAutonomy widened a read_only session to autonomous")
	}
	if f.sessions[s.SessionID].AutonomyLevel != "read_only" || len(f.events) != 0 {
		t.Errorf("a refused widening changed level to %s and recorded %v", f.sessions[s.SessionID].AutonomyLevel, f.eventTypes())
	}
	if err := c.TightenAutonomy(context.Background(), tenant, s.SessionID, "yolo"); err == nil {
		t.Error("TightenAutonomy accepted an unknown level")
	}
}

func TestResumeConversation_AppendsTheMessageOnceAndQueuesTheContinuation(t *testing.T) {
	c, f, q := newControl()
	s := f.seedSession(eventlog.SessionStatusAwaitingInput, "supervised")

	if _, err := c.ResumeConversation(context.Background(), tenant, s.SessionID, "and then?"); err != nil {
		t.Fatalf("ResumeConversation: %v", err)
	}
	if f.sessions[s.SessionID].Status != eventlog.SessionStatusRunning {
		t.Errorf("status = %s, want running", f.sessions[s.SessionID].Status)
	}
	if len(q.jobs) != 1 || q.jobs[0].SessionID != s.SessionID {
		t.Errorf("queued jobs = %+v, want exactly one continuation for the session", q.jobs)
	}

	// A second caller racing the first finds the session already claimed.
	if _, err := c.ResumeConversation(context.Background(), tenant, s.SessionID, "and then?"); err == nil {
		t.Fatal("a second resume of the same awaiting turn succeeded: the message would be appended twice")
	}
	if len(q.jobs) != 1 || len(f.events) != 1 {
		t.Errorf("after the losing call: %d jobs, %d events, want 1 and 1", len(q.jobs), len(f.events))
	}
}

func TestEndIdleConversation_OnlyEndsASessionStillWaiting(t *testing.T) {
	c, f, _ := newControl()
	waiting := f.seedSession(eventlog.SessionStatusAwaitingInput, "supervised")
	answered := f.seedSession(eventlog.SessionStatusRunning, "supervised")

	if err := c.EndIdleConversation(context.Background(), tenant, waiting.SessionID); err != nil {
		t.Fatalf("EndIdleConversation: %v", err)
	}
	got := f.sessions[waiting.SessionID]
	if got.Status != eventlog.SessionStatusFailed || got.TerminalReason == nil || *got.TerminalReason != "idle_timeout" {
		t.Errorf("waiting session = %s reason %v, want failed with idle_timeout (so a golden-signal query tells it from a user stop)", got.Status, got.TerminalReason)
	}

	if err := c.EndIdleConversation(context.Background(), tenant, answered.SessionID); err != nil {
		t.Fatalf("EndIdleConversation of a session that moved on: %v", err)
	}
	if f.sessions[answered.SessionID].Status != eventlog.SessionStatusRunning {
		t.Errorf("a session that had already been resumed was ended by the sweep")
	}
	if len(f.events) != 1 {
		t.Errorf("events = %v, want only the one terminal event for the idle session", f.eventTypes())
	}
}

func TestClaimTracker_RecordsTheClaimWithItsEventAndRefusesAnAmbiguousRetry(t *testing.T) {
	f := newFakeStore()
	tr := runctl.NewClaimTracker(f)
	session, digest := uuid.New(), []byte("digest-1")

	id, outcome, err := tr.Open(context.Background(), tenant, session, "platform/email@v1", digest)
	if err != nil || outcome != tools.ClaimFresh {
		t.Fatalf("first Open = %v, %v, want a fresh claim", outcome, err)
	}
	if types := f.eventTypes(); len(types) != 1 || types[0] != eventlog.EventEffectClaimed {
		t.Errorf("events = %v, want effect_claimed with the claim", types)
	}

	// The same call again while the first is unresolved: never re-execute.
	if _, outcome, err = tr.Open(context.Background(), tenant, session, "platform/email@v1", digest); err != nil || outcome != tools.ClaimAmbiguous {
		t.Errorf("second Open = %v, %v, want ambiguous", outcome, err)
	}
	if len(f.events) != 1 {
		t.Errorf("%d events after a refused duplicate, want still 1", len(f.events))
	}

	if err := tr.Complete(context.Background(), tenant, session, id, false, ""); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, outcome, _ = tr.Open(context.Background(), tenant, session, "platform/email@v1", digest); outcome != tools.ClaimDone {
		t.Errorf("Open after a completed call = %v, want done: a completed effect is never run twice", outcome)
	}
}

func TestClaimTracker_AFailedCallAbandonsTheClaim(t *testing.T) {
	f := newFakeStore()
	tr := runctl.NewClaimTracker(f)
	session := uuid.New()
	id, _, _ := tr.Open(context.Background(), tenant, session, "t", []byte("d"))

	if err := tr.Complete(context.Background(), tenant, session, id, true, "tool errored before producing output"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := f.claims[id]; got.Status != runctl.ClaimAbandoned || got.Reason == "" {
		t.Errorf("claim = %s %q, want abandoned with its reason", got.Status, got.Reason)
	}
}

func TestClaimTracker_OpeningAClaimIsAllOrNothing(t *testing.T) {
	f := newFakeStore()
	tr := runctl.NewClaimTracker(f)
	f.failAppend = errors.New("audit chain unavailable")

	if _, _, err := tr.Open(context.Background(), tenant, uuid.New(), "t", []byte("d")); err == nil {
		t.Fatal("Open succeeded though effect_claimed could not be recorded")
	}
	if len(f.claims) != 0 {
		t.Errorf("%d claims survive a failed open, want none: a claim without its event is a write-ahead record no one can audit", len(f.claims))
	}
}

func TestResume_RefusesWhileAClaimIsUnresolved(t *testing.T) {
	c, f, _ := newControl()
	s := f.seedSession(eventlog.SessionStatusRunning, "supervised")
	tr := runctl.NewClaimTracker(f)
	if _, _, err := tr.Open(context.Background(), tenant, s.SessionID, "platform/email@v1", []byte("d")); err != nil {
		t.Fatalf("Open: %v", err)
	}

	var got error
	for _, err := range c.Resume(context.Background(), tenant, s.SessionID) {
		got = err
		break
	}
	var unresolved runctl.ErrUnresolvedClaims
	if !errors.As(got, &unresolved) || len(unresolved.ClaimIDs) != 1 {
		t.Fatalf("Resume = %v, want ErrUnresolvedClaims naming the in-flight claim: re-entering the loop could repeat an external effect", got)
	}
}

func TestResolveClaim_RecordsTheResolutionWithItsEvent(t *testing.T) {
	c, f, _ := newControl()
	tr := runctl.NewClaimTracker(f)
	session := uuid.New()
	id, _, _ := tr.Open(context.Background(), tenant, session, "t", []byte("d"))

	got, err := c.ResolveClaim(context.Background(), tenant, session, id, runctl.ClaimAbandoned, "operator confirmed the email was never sent")
	if err != nil || got.Status != runctl.ClaimAbandoned {
		t.Fatalf("ResolveClaim = %+v, %v", got, err)
	}
	if types := f.eventTypes(); len(types) != 2 || types[1] != eventlog.EventEffectClaimResolved {
		t.Errorf("events = %v, want the claim then its resolution", types)
	}
}
