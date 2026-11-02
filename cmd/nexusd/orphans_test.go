package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/queue"
)

// recordingQueue is a queue.Port that only records Enqueue; the sweep uses
// nothing else.
type recordingQueue struct {
	mu       sync.Mutex
	jobs     []queue.Job
	failFor  map[uuid.UUID]bool // session ids whose Enqueue fails
	enqueues int
}

func (q *recordingQueue) Enqueue(_ context.Context, job queue.Job) (queue.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueues++
	if q.failFor[job.SessionID] {
		return queue.Job{}, errors.New("redis: connection refused")
	}
	q.jobs = append(q.jobs, job)
	return job, nil
}
func (*recordingQueue) Lease(context.Context, string, time.Duration) (queue.Job, bool, error) {
	return queue.Job{}, false, nil
}
func (*recordingQueue) Complete(context.Context, uuid.UUID) error { return nil }
func (*recordingQueue) Fail(context.Context, uuid.UUID, string, bool, time.Time) error {
	return nil
}

func (q *recordingQueue) enqueuedSessions() []uuid.UUID {
	q.mu.Lock()
	defer q.mu.Unlock()
	var ids []uuid.UUID
	for _, j := range q.jobs {
		ids = append(ids, j.SessionID)
	}
	return ids
}

// fakeLiveness answers Held from a set of session keys, or with an error for
// the keys in broken.
type fakeLiveness struct {
	mu     sync.Mutex
	held   map[string]bool
	broken map[string]bool
}

func (f *fakeLiveness) Held(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken[key] {
		return false, errors.New("redis: timeout")
	}
	return f.held[key], nil
}

func candidate(key string) orphanCandidate {
	return orphanCandidate{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: key}
}

// With more than one process, "running in the database" no longer means
// orphaned: a session another process's worker is driving holds its lock.
func TestRecoverOrphans_SkipsSessionsAWorkerIsDriving(t *testing.T) {
	a, b, c := candidate("a"), candidate("b"), candidate("c")
	q := &recordingQueue{}
	live := &fakeLiveness{held: map[string]bool{"b": true}}

	got := recoverOrphans(context.Background(), "test", []orphanCandidate{a, b, c}, live, q)

	if got != 2 {
		t.Errorf("recovered = %d, want 2", got)
	}
	ids := q.enqueuedSessions()
	if len(ids) != 2 || ids[0] != a.SessionID || ids[1] != c.SessionID {
		t.Errorf("enqueued %v, want resumes for a and c only (b is live)", ids)
	}
	for _, j := range q.jobs {
		if j.Kind != queue.KindResume {
			t.Errorf("job kind = %q, want resume", j.Kind)
		}
	}
	if q.jobs[0].TenantID != a.TenantID || q.jobs[0].SessionKey != "a" {
		t.Errorf("job %+v does not carry the candidate's tenant and session key", q.jobs[0])
	}
}

// If liveness cannot be read, the session might be live: leave it, and let the
// recheck decide, rather than resume a run that may be mid-turn.
func TestRecoverOrphans_LeavesASessionWhoseLivenessIsUnknown(t *testing.T) {
	a, b := candidate("a"), candidate("b")
	q := &recordingQueue{}
	live := &fakeLiveness{broken: map[string]bool{"b": true}}

	got := recoverOrphans(context.Background(), "test", []orphanCandidate{a, b}, live, q)

	if ids := q.enqueuedSessions(); got != 1 || len(ids) != 1 || ids[0] != a.SessionID {
		t.Errorf("recovered %d, enqueued %v: want only a", got, ids)
	}
}

func TestRecoverOrphans_ContinuesPastAFailedEnqueue(t *testing.T) {
	a, b := candidate("a"), candidate("b")
	q := &recordingQueue{failFor: map[uuid.UUID]bool{a.SessionID: true}}

	got := recoverOrphans(context.Background(), "test", []orphanCandidate{a, b}, &fakeLiveness{}, q)

	if ids := q.enqueuedSessions(); got != 1 || len(ids) != 1 || ids[0] != b.SessionID {
		t.Errorf("recovered %d, enqueued %v: a failed enqueue must not stop the sweep", got, ids)
	}
}

// expiresAfterFirstCheck reports a session as held the first time it is asked
// and gone ever after: a dead worker's lock, seen once before its TTL passes.
type expiresAfterFirstCheck struct{ calls atomic.Int32 }

func (e *expiresAfterFirstCheck) Held(context.Context, string) (bool, error) {
	return e.calls.Add(1) == 1, nil
}

// A worker that just died leaves its lock behind until the TTL passes, so the
// first sweep must skip that session and the recheck must pick it up. It is one
// recheck, not a loop.
func TestStartOrphanRecovery_RechecksOnceAfterTheDelay(t *testing.T) {
	orphan := candidate("dead-workers-session")
	q := &recordingQueue{}
	var lists atomic.Int32
	list := func(context.Context) ([]orphanCandidate, error) {
		lists.Add(1)
		return []orphanCandidate{orphan}, nil
	}

	startOrphanRecovery(context.Background(), list, &expiresAfterFirstCheck{}, q, 20*time.Millisecond)

	if ids := q.enqueuedSessions(); len(ids) != 0 {
		t.Fatalf("the startup sweep enqueued %v for a session whose lock was still held", ids)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(q.enqueuedSessions()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ids := q.enqueuedSessions(); len(ids) != 1 || ids[0] != orphan.SessionID {
		t.Fatalf("after the recheck enqueued %v, want one resume for the orphan", ids)
	}

	time.Sleep(100 * time.Millisecond) // five more recheck intervals: it must not keep sweeping
	if n := lists.Load(); n != 2 {
		t.Errorf("listed running sessions %d times, want exactly 2 (startup + one recheck): a periodic loop would re-enqueue a permanently failing orphan forever", n)
	}
}

func TestStartOrphanRecovery_NoRecheckOnceTheContextIsCancelled(t *testing.T) {
	var lists atomic.Int32
	list := func(context.Context) ([]orphanCandidate, error) {
		lists.Add(1)
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	startOrphanRecovery(ctx, list, &fakeLiveness{}, &recordingQueue{}, 30*time.Millisecond)
	cancel() // shutdown before the recheck is due

	time.Sleep(120 * time.Millisecond)
	if n := lists.Load(); n != 1 {
		t.Errorf("listed %d times, want 1: the recheck ran after the context was cancelled", n)
	}
}
