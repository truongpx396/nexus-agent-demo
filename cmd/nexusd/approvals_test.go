package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	oversightpg "github.com/truongpx396/nexus-agent-demo/internal/oversight/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/queue"
	"github.com/truongpx396/nexus-agent-demo/internal/reliability"
)

// flakyQueue fails the first failFor enqueues, then accepts.
type flakyQueue struct {
	failFor int
	calls   int
	jobs    []queue.Job
}

func (q *flakyQueue) Enqueue(_ context.Context, job queue.Job) (queue.Job, error) {
	q.calls++
	if q.calls <= q.failFor {
		return queue.Job{}, errors.New("redis: connection refused")
	}
	q.jobs = append(q.jobs, job)
	return job, nil
}
func (*flakyQueue) Lease(context.Context, string, time.Duration) (queue.Job, bool, error) {
	return queue.Job{}, false, nil
}
func (*flakyQueue) Complete(context.Context, uuid.UUID) error { return nil }
func (*flakyQueue) Fail(context.Context, uuid.UUID, string, bool, time.Time) error {
	return nil
}

func TestApprovalScheduler_QueuesAnApprovalJobThatCarriesOnlyIds(t *testing.T) {
	q := &flakyQueue{}
	tenant, session, approval := uuid.New(), uuid.New(), uuid.New()
	if err := (approvalScheduler{queue: q}).ScheduleResume(context.Background(), tenant, session, approval); err != nil {
		t.Fatalf("ScheduleResume: %v", err)
	}
	if len(q.jobs) != 1 {
		t.Fatalf("%d jobs queued, want one", len(q.jobs))
	}
	job := q.jobs[0]
	if job.Kind != queue.KindApproval || job.TenantID != tenant || job.SessionID != session || job.SessionKey != session.String() {
		t.Fatalf("job = %+v, want an approval job locked on the session id like every worker job", job)
	}
	var p approvalJobPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.ApprovalID != approval {
		t.Fatalf("payload %q, want the approval id and nothing else", job.Payload)
	}
}

func TestApprovalScheduler_RetriesABriefOutageThenGivesUp(t *testing.T) {
	brief := &flakyQueue{failFor: 2}
	if err := (approvalScheduler{queue: brief}).ScheduleResume(context.Background(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("a two-call outage should be ridden out: %v", err)
	}
	down := &flakyQueue{failFor: 99}
	if err := (approvalScheduler{queue: down}).ScheduleResume(context.Background(), uuid.New(), uuid.New(), uuid.New()); err == nil {
		t.Fatal("a queue that never answers was reported as scheduled")
	}
	if down.calls != scheduleAttempts {
		t.Fatalf("%d attempts, want %d", down.calls, scheduleAttempts)
	}
}

func TestJobEvents_AnApprovalJobWithNoApprovalIDFailsForGood(t *testing.T) {
	job := queue.Job{JobID: uuid.New(), Kind: queue.KindApproval, Payload: json.RawMessage(`{}`)}
	var got error
	for _, err := range jobEvents(context.Background(), nil, nil, job) {
		got = err
	}
	if _, ok := errors.AsType[*reliability.PermanentError](got); !ok {
		t.Fatalf("err = %v, want a permanent failure: retrying a job with no approval id can never succeed", got)
	}
}

func TestUnresumedOrphans_BecomeApprovalJobsOnTheSessionLock(t *testing.T) {
	u := oversightpg.Unresumed{TenantID: uuid.New(), SessionID: uuid.New(), ApprovalID: uuid.New()}
	got, err := unresumedOrphans([]oversightpg.Unresumed{u})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	o := got[0]
	if o.Kind != queue.KindApproval || o.SessionKey != u.SessionID.String() || o.TenantID != u.TenantID {
		t.Fatalf("orphan = %+v", o)
	}
	var p approvalJobPayload
	if err := json.Unmarshal(o.Payload, &p); err != nil || p.ApprovalID != u.ApprovalID {
		t.Fatalf("payload %q, want the approval", o.Payload)
	}
}

// The errors retrying cannot fix are permanent, and a transient one is not.
func TestPermanentWhenFutile(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		permanent bool
	}{
		"not decided": {oversight.ErrNotDecided, true},
		"not found":   {oversight.ErrNotFound, true},
		"transient":   {errors.New("db timeout"), false},
	} {
		var got error
		for _, err := range permanentWhenFutile(failedSeq(tc.err)) {
			got = err
		}
		_, perm := errors.AsType[*reliability.PermanentError](got)
		if perm != tc.permanent {
			t.Errorf("%s: permanent = %v, want %v", name, perm, tc.permanent)
		}
	}
}
