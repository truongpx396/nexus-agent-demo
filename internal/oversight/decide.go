package oversight

import (
	"context"
	"encoding/json"
	"iter"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// Outcome summarizes what deciding an approval actually did: the session the
// decision resumed and how many events the resumed run appended. It does not
// repeat the run's status, which the run's own read endpoint answers; it is only
// the synchronous acknowledgment that the resume happened. Err is set, and the
// counts stop, when the resumed run failed part-way.
type Outcome struct {
	SessionID      string
	EventsAppended int
	Err            string
}

// ResumeSource is the part of Resumer a decision needs: resuming a suspended run
// with a grant, a modified grant or a denial, as a stream of the events the run
// appends. *Resumer satisfies it.
type ResumeSource interface {
	Grant(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string) iter.Seq2[eventlog.Event, error]
	GrantModified(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string, modifiedInput json.RawMessage) iter.Seq2[eventlog.Event, error]
	Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) iter.Seq2[eventlog.Event, error]
}

// SettledNotifier is told when a decision's resume has left a session as far
// along as it will go (runctl.Control.NotifySettled). The session an approval
// resumed may also be a delegation's child or a team member, and whoever cares
// is an observer behind this, so oversight neither names nor imports them.
type SettledNotifier interface {
	NotifySettled(ctx context.Context, tenantID, sessionID uuid.UUID)
}

// Decisions decides approvals: it resumes the suspended run and waits for it to
// finish or suspend again. Unlike a fresh run, whose caller gets a stream back
// at once, an approval decision's caller waits for the resumed run, which is the
// trade the approval endpoints make for a response that says what happened.
type Decisions struct {
	Resume ResumeSource
	// Settled is optional: nil means nothing is told when a resume settles.
	Settled SettledNotifier
}

// Grant approves an approval as proposed and resumes the run.
func (d Decisions) Grant(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string) Outcome {
	return d.settle(ctx, tenantID, drain(d.Resume.Grant(ctx, tenantID, approvalID, decidedBy)))
}

// GrantModified approves an approval with the approver's substitute for the
// tool input and resumes the run.
func (d Decisions) GrantModified(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string, modifiedInput json.RawMessage) Outcome {
	return d.settle(ctx, tenantID, drain(d.Resume.GrantModified(ctx, tenantID, approvalID, decidedBy, modifiedInput)))
}

// Deny refuses an approval and resumes the run with the refusal.
func (d Decisions) Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) Outcome {
	return d.settle(ctx, tenantID, drain(d.Resume.Deny(ctx, tenantID, approvalID, decidedBy, reason)))
}

// settle is the approval path's counterpart of the queue worker's: a session an
// ordinary tool approval just resumed may also be a delegation's child (README
// task 8.10) or a team member (task 9.9). A failed resume settles nothing.
func (d Decisions) settle(ctx context.Context, tenantID uuid.UUID, out Outcome) Outcome {
	if d.Settled == nil || out.Err != "" || out.SessionID == "" {
		return out
	}
	if sessionID, err := uuid.Parse(out.SessionID); err == nil {
		d.Settled.NotifySettled(ctx, tenantID, sessionID)
	}
	return out
}

// drain fully consumes a resume stream into an Outcome, stopping at the first
// error the run yields.
func drain(events iter.Seq2[eventlog.Event, error]) Outcome {
	var out Outcome
	for ev, err := range events {
		if err != nil {
			out.Err = err.Error()
			return out
		}
		out.SessionID = ev.SessionID.String()
		out.EventsAppended++
	}
	return out
}
