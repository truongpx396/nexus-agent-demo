package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent/internal/eventlog"
	"github.com/truongpx396/nexus-agent/internal/oversight"
	oversightpg "github.com/truongpx396/nexus-agent/internal/oversight/postgres"
	"github.com/truongpx396/nexus-agent/internal/queue"
	"github.com/truongpx396/nexus-agent/internal/reliability"
	"github.com/truongpx396/nexus-agent/internal/runctl"
)

// approvalJobPayload is what a queue.KindApproval job carries: only the id, never
// the tool's input, which stays sealed in the log.
type approvalJobPayload struct {
	ApprovalID uuid.UUID `json:"approval_id"`
}

// scheduleAttempts and scheduleBackoff bound how hard ScheduleResume tries to
// reach the queue before it gives up and leaves the approval to recovery.
const (
	scheduleAttempts = 3
	scheduleBackoff  = 100 * time.Millisecond
)

// approvalScheduler is the oversight.ResumeScheduler this binary ships. A decided
// approval becomes a queue job, so the run it suspended continues on a worker
// holding the session lock, publishing to the event bus, like every other run.
type approvalScheduler struct{ queue queue.Port }

var _ oversight.ResumeScheduler = approvalScheduler{}

func (a approvalScheduler) ScheduleResume(ctx context.Context, tenantID, sessionID, approvalID uuid.UUID) error {
	job, err := approvalJob(tenantID, sessionID, approvalID)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := range scheduleAttempts {
		if _, lastErr = a.queue.Enqueue(ctx, job); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("enqueue approval resume: %w", ctx.Err())
		case <-time.After(scheduleBackoff * time.Duration(attempt+1)):
		}
	}
	return fmt.Errorf("enqueue approval resume: %w", lastErr)
}

func approvalJob(tenantID, sessionID, approvalID uuid.UUID) (queue.Job, error) {
	payload, err := json.Marshal(approvalJobPayload{ApprovalID: approvalID})
	if err != nil {
		return queue.Job{}, fmt.Errorf("marshal approval job payload: %w", err)
	}
	return queue.Job{TenantID: tenantID, SessionID: sessionID, SessionKey: sessionID.String(), Kind: queue.KindApproval, Payload: payload}, nil
}

// approvalResumer is the part of *oversight.Resumer a worker needs.
type approvalResumer interface {
	ResumeDecided(ctx context.Context, tenantID, approvalID uuid.UUID) iter.Seq2[eventlog.Event, error]
}

// jobEvents is the run a queue job continues: an approval job resumes the run the
// approval suspended, every other kind goes through runctl.Control.Resume. A job
// that can never succeed (a payload that is not an approval id, an approval that
// is not decided or does not exist) fails permanently instead of retrying.
func jobEvents(ctx context.Context, ctl *runctl.Control, approvals approvalResumer, job queue.Job) iter.Seq2[eventlog.Event, error] {
	if job.Kind != queue.KindApproval {
		return ctl.Resume(ctx, job.TenantID, job.SessionID)
	}
	var p approvalJobPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.ApprovalID == uuid.Nil {
		return failedSeq(&reliability.PermanentError{Err: fmt.Errorf("approval job %s has no approval id in its payload", job.JobID)})
	}
	return permanentWhenFutile(approvals.ResumeDecided(ctx, job.TenantID, p.ApprovalID))
}

// permanentWhenFutile marks the errors retrying cannot fix as permanent.
func permanentWhenFutile(seq iter.Seq2[eventlog.Event, error]) iter.Seq2[eventlog.Event, error] {
	return func(yield func(eventlog.Event, error) bool) {
		for ev, err := range seq {
			if err != nil && (errors.Is(err, oversight.ErrNotDecided) || errors.Is(err, oversight.ErrNotFound)) {
				err = &reliability.PermanentError{Err: err}
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

func failedSeq(err error) iter.Seq2[eventlog.Event, error] {
	return func(yield func(eventlog.Event, error) bool) { yield(eventlog.Event{}, err) }
}

// unresumedOrphans turns the approvals decided but never resumed into the jobs
// that resume them.
func unresumedOrphans(found []oversightpg.Unresumed) ([]runctl.Orphan, error) {
	out := make([]runctl.Orphan, 0, len(found))
	for _, u := range found {
		job, err := approvalJob(u.TenantID, u.SessionID, u.ApprovalID)
		if err != nil {
			return nil, err
		}
		out = append(out, runctl.Orphan{TenantID: u.TenantID, SessionID: u.SessionID, SessionKey: job.SessionKey, Kind: job.Kind, Payload: job.Payload})
	}
	return out, nil
}
