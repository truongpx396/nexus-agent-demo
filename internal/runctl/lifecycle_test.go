package runctl_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
)

// A checkpoint is a pointer into state that lives elsewhere: how far the log
// goes, the oldest claim nobody resolved, and the harness the session was
// pinned to. It must not invent any of them.
func TestSaveCheckpoint_PointsAtTheLogTheOpenClaimAndTheHarness(t *testing.T) {
	c, f, _ := newControl()
	s := f.seedSession(eventlog.SessionStatusRunning, "supervised")
	s.HarnessDigest = []byte("harness-digest")
	f.sessions[s.SessionID] = s
	f.history[s.SessionID] = []eventlog.Event{{Seq: 1}, {Seq: 2}, {Seq: 7}}
	claim := runctl.Claim{ClaimID: uuid.New(), SessionID: s.SessionID, Status: runctl.ClaimInFlight}
	f.claims[claim.ClaimID] = claim

	if err := c.SaveCheckpoint(context.Background(), tenant, s.SessionID); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	if len(f.checkpoints) != 1 {
		t.Fatalf("checkpoints = %d, want 1", len(f.checkpoints))
	}
	got := f.checkpoints[0]
	if got.SessionID != s.SessionID || got.TenantID != tenant {
		t.Errorf("checkpoint is for %v/%v, want %v/%v", got.TenantID, got.SessionID, tenant, s.SessionID)
	}
	if got.CoveredSeq != 7 {
		t.Errorf("CoveredSeq = %d, want 7 (the last event in the log)", got.CoveredSeq)
	}
	if got.OpenClaimID == nil || *got.OpenClaimID != claim.ClaimID {
		t.Errorf("OpenClaimID = %v, want the in-flight claim %v", got.OpenClaimID, claim.ClaimID)
	}
	if string(got.HarnessDigest) != "harness-digest" {
		t.Errorf("HarnessDigest = %q, want the session's pinned digest", got.HarnessDigest)
	}
}

func TestSaveCheckpoint_OfASessionWithNothingOpenCoversNothing(t *testing.T) {
	c, f, _ := newControl()
	s := f.seedSession(eventlog.SessionStatusCompleted, "supervised")

	if err := c.SaveCheckpoint(context.Background(), tenant, s.SessionID); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	got := f.checkpoints[0]
	if got.CoveredSeq != 0 || got.OpenClaimID != nil {
		t.Errorf("checkpoint = seq %d claim %v, want 0 and nil for an empty, claim-free session", got.CoveredSeq, got.OpenClaimID)
	}
}

func TestSaveCheckpoint_OfAMissingSessionWritesNothing(t *testing.T) {
	c, f, _ := newControl()
	if err := c.SaveCheckpoint(context.Background(), tenant, uuid.New()); err == nil {
		t.Fatal("SaveCheckpoint of a session that does not exist succeeded")
	}
	if len(f.checkpoints) != 0 {
		t.Errorf("checkpoints = %d, want none", len(f.checkpoints))
	}
}

// The sweep ends exactly the conversations that have been quiet past the
// window, and leaves the recent and the already-answered alone.
func TestSweepIdle_EndsOnlyConversationsQuietPastTheWindow(t *testing.T) {
	c, f, _ := newControl()
	stale := f.seedSession(eventlog.SessionStatusAwaitingInput, "supervised")
	stale.UpdatedAt = time.Now().Add(-2 * time.Hour)
	f.sessions[stale.SessionID] = stale
	recent := f.seedSession(eventlog.SessionStatusAwaitingInput, "supervised")
	recent.UpdatedAt = time.Now()
	f.sessions[recent.SessionID] = recent
	running := f.seedSession(eventlog.SessionStatusRunning, "supervised")
	running.UpdatedAt = time.Now().Add(-2 * time.Hour)
	f.sessions[running.SessionID] = running

	if err := c.SweepIdle(context.Background(), tenant, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("SweepIdle: %v", err)
	}

	if got := f.sessions[stale.SessionID].Status; got != eventlog.SessionStatusFailed {
		t.Errorf("stale conversation = %s, want failed", got)
	}
	if got := f.sessions[recent.SessionID].Status; got != eventlog.SessionStatusAwaitingInput {
		t.Errorf("recent conversation = %s, want it left awaiting input", got)
	}
	if got := f.sessions[running.SessionID].Status; got != eventlog.SessionStatusRunning {
		t.Errorf("a running session = %s, want it left running", got)
	}
}

type recordingObserver struct {
	err   error
	calls []uuid.UUID
}

func (o *recordingObserver) OnSessionSettled(_ context.Context, _, sessionID uuid.UUID) error {
	o.calls = append(o.calls, sessionID)
	return o.err
}

// One path forgetting an observer is how a delegation's parent went on waiting
// for a child that had already ended: every observer hears every settled
// session, and a broken one must not deafen the rest.
func TestNotifySettled_TellsEveryObserverEvenWhenOneFails(t *testing.T) {
	broken := &recordingObserver{err: errors.New("db down")}
	healthy := &recordingObserver{}
	c, _, _ := newControl()
	c.Observers = []runctl.SettledObserver{broken, healthy}
	id := uuid.New()

	c.NotifySettled(context.Background(), tenant, id)

	if len(broken.calls) != 1 || len(healthy.calls) != 1 || healthy.calls[0] != id {
		t.Errorf("broken heard %v, healthy heard %v; both should hear %v", broken.calls, healthy.calls, id)
	}
}

func TestNotifySettled_WithNoObserversIsHarmless(t *testing.T) {
	c, _, _ := newControl()
	c.NotifySettled(context.Background(), tenant, uuid.New()) // must not panic
}
