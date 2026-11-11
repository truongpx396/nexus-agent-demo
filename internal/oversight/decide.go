package oversight

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/rs/zerolog/log"
)

// Decider decides one approval. *Approvals satisfies it.
type Decider interface {
	Grant(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string) (Approval, error)
	GrantModified(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string, modifiedInput json.RawMessage) (Approval, error)
	Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) (Approval, error)
}

// ResumeScheduler hands a decided approval to whatever drives runs, so the run it
// suspended continues. cmd/nexusd implements it by enqueueing a job: the resumed
// run then takes the same session lock, publishes to the same event bus, and is
// retried and recovered the same way as every other run, instead of running
// inside an HTTP request that holds no lock, publishes nothing and dies with the
// connection.
type ResumeScheduler interface {
	ScheduleResume(ctx context.Context, tenantID, sessionID, approvalID uuid.UUID) error
}

// Outcome is what deciding an approval did. The decision itself is durable. The
// resume is scheduled, not waited for: Resuming says whether it was handed to
// the queue.
type Outcome struct {
	ApprovalID uuid.UUID
	SessionID  uuid.UUID

	// Resuming is false when the decision was recorded but the hand-off failed.
	// That is not lost work: the decision stays on the approval, and recovery
	// resumes a suspended run whose approval was decided and never resumed.
	Resuming bool
}

// Decisions decides approvals and schedules the resume of the run each one
// suspended.
type Decisions struct {
	Approvals Decider
	Resume    ResumeScheduler
}

// Grant approves an approval as proposed.
func (d Decisions) Grant(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string) (Outcome, error) {
	ap, err := d.Approvals.Grant(ctx, tenantID, approvalID, decidedBy)
	return d.schedule(ctx, ap, err)
}

// GrantModified approves an approval with the approver's substitute for the tool
// input, which re-binds the digest the call is executed against.
func (d Decisions) GrantModified(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string, modifiedInput json.RawMessage) (Outcome, error) {
	ap, err := d.Approvals.GrantModified(ctx, tenantID, approvalID, decidedBy, modifiedInput)
	return d.schedule(ctx, ap, err)
}

// Deny refuses an approval.
func (d Decisions) Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) (Outcome, error) {
	ap, err := d.Approvals.Deny(ctx, tenantID, approvalID, decidedBy, reason)
	return d.schedule(ctx, ap, err)
}

func (d Decisions) schedule(ctx context.Context, ap Approval, err error) (Outcome, error) {
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{ApprovalID: ap.ApprovalID, SessionID: ap.SessionID}
	if serr := d.Resume.ScheduleResume(ctx, ap.TenantID, ap.SessionID, ap.ApprovalID); serr != nil {
		log.Error().Err(serr).Any("approval_id", ap.ApprovalID).Any("session_id", ap.SessionID).
			Msg("oversight: approval decided but its resume could not be queued; recovery will pick it up")
		return out, nil
	}
	out.Resuming = true
	return out, nil
}
