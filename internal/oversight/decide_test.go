package oversight_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
)

// fakeDecider returns a fixed approval for whichever decision is taken and
// records which one it was.
type fakeDecider struct {
	approval oversight.Approval
	err      error
	called   string
	modified json.RawMessage
	reason   string
}

func (f *fakeDecider) Grant(context.Context, uuid.UUID, uuid.UUID, string) (oversight.Approval, error) {
	f.called = "grant"
	return f.approval, f.err
}

func (f *fakeDecider) GrantModified(_ context.Context, _, _ uuid.UUID, _ string, modified json.RawMessage) (oversight.Approval, error) {
	f.called, f.modified = "grant_modified", modified
	return f.approval, f.err
}

func (f *fakeDecider) Deny(_ context.Context, _, _ uuid.UUID, _, reason string) (oversight.Approval, error) {
	f.called, f.reason = "deny", reason
	return f.approval, f.err
}

// fakeScheduler records the resumes it was asked to schedule.
type fakeScheduler struct {
	err  error
	got  []uuid.UUID // the session of each scheduled resume
	appr []uuid.UUID
}

func (f *fakeScheduler) ScheduleResume(_ context.Context, _, sessionID, approvalID uuid.UUID) error {
	f.got = append(f.got, sessionID)
	f.appr = append(f.appr, approvalID)
	return f.err
}

func decided(tenant uuid.UUID) oversight.Approval {
	return oversight.Approval{ApprovalID: uuid.New(), TenantID: tenant, SessionID: uuid.New()}
}

func TestDecisions_EachDecisionIsRecordedThenItsResumeIsScheduled(t *testing.T) {
	ctx, tenant := context.Background(), uuid.New()
	for _, tc := range []struct {
		want string
		run  func(d oversight.Decisions) (oversight.Outcome, error)
	}{
		{"grant", func(d oversight.Decisions) (oversight.Outcome, error) {
			return d.Grant(ctx, tenant, uuid.New(), "alice")
		}},
		{"grant_modified", func(d oversight.Decisions) (oversight.Outcome, error) {
			return d.GrantModified(ctx, tenant, uuid.New(), "alice", json.RawMessage(`{"to":"bob"}`))
		}},
		{"deny", func(d oversight.Decisions) (oversight.Outcome, error) {
			return d.Deny(ctx, tenant, uuid.New(), "alice", "no")
		}},
	} {
		ap := decided(tenant)
		decider, sched := &fakeDecider{approval: ap}, &fakeScheduler{}
		out, err := tc.run(oversight.Decisions{Approvals: decider, Resume: sched})
		if err != nil {
			t.Fatalf("%s: %v", tc.want, err)
		}
		if decider.called != tc.want {
			t.Errorf("%s: decided with %q", tc.want, decider.called)
		}
		if len(sched.got) != 1 || sched.got[0] != ap.SessionID || sched.appr[0] != ap.ApprovalID {
			t.Errorf("%s: scheduled %v for approvals %v, want the one resume of the approval's own session", tc.want, sched.got, sched.appr)
		}
		if !out.Resuming || out.SessionID != ap.SessionID || out.ApprovalID != ap.ApprovalID {
			t.Errorf("%s: outcome = %+v, want it to name the approval and its session and say it is resuming", tc.want, out)
		}
	}
}

// A decision that was not recorded must not wake the run: an approval already
// decided, or one that is not there, resumes nothing.
func TestDecisions_ARefusedDecisionSchedulesNothing(t *testing.T) {
	notPending := oversight.ApprovalNotPendingError{ApprovalID: uuid.New(), Status: oversight.ApprovalGranted}
	sched := &fakeScheduler{}
	_, err := oversight.Decisions{Approvals: &fakeDecider{err: notPending}, Resume: sched}.Grant(context.Background(), uuid.New(), uuid.New(), "alice")
	if !errors.Is(err, notPending) {
		t.Fatalf("err = %v, want the refusal passed through", err)
	}
	if len(sched.got) != 0 {
		t.Fatalf("scheduled %v after a refused decision", sched.got)
	}
}

// The decision is durable once it commits. If the hand-off to the queue fails the
// caller still gets an answer, and recovery resumes the run.
func TestDecisions_AFailedHandOffKeepsTheDecisionAndSaysTheRunIsNotResuming(t *testing.T) {
	ap := decided(uuid.New())
	out, err := oversight.Decisions{Approvals: &fakeDecider{approval: ap}, Resume: &fakeScheduler{err: errors.New("redis down")}}.
		Grant(context.Background(), ap.TenantID, ap.ApprovalID, "alice")
	if err != nil {
		t.Fatalf("a recorded decision was reported as failed: %v", err)
	}
	if out.Resuming || out.SessionID != ap.SessionID {
		t.Fatalf("outcome = %+v, want the session named and Resuming false", out)
	}
}
