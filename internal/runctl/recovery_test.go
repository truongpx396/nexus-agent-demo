package runctl_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent/internal/queue"
	"github.com/truongpx396/nexus-agent/internal/runctl"
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

func candidate(key string) runctl.Orphan {
	return runctl.Orphan{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: key}
}

// With more than one process, "running in the database" no longer means
// orphaned: a session another process's worker is driving holds its lock.
func TestRecoverOrphans_SkipsSessionsAWorkerIsDriving(t *testing.T) {
	a, b, c := candidate("a"), candidate("b"), candidate("c")
	q := &recordingQueue{}
	live := &fakeLiveness{held: map[string]bool{"b": true}}

	got := runctl.RecoverOrphans(context.Background(), "test", []runctl.Orphan{a, b, c}, live, q, nil)

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

	got := runctl.RecoverOrphans(context.Background(), "test", []runctl.Orphan{a, b}, live, q, nil)

	if ids := q.enqueuedSessions(); got != 1 || len(ids) != 1 || ids[0] != a.SessionID {
		t.Errorf("recovered %d, enqueued %v: want only a", got, ids)
	}
}

func TestRecoverOrphans_ContinuesPastAFailedEnqueue(t *testing.T) {
	a, b := candidate("a"), candidate("b")
	q := &recordingQueue{failFor: map[uuid.UUID]bool{a.SessionID: true}}

	got := runctl.RecoverOrphans(context.Background(), "test", []runctl.Orphan{a, b}, &fakeLiveness{}, q, nil)

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
	list := func(context.Context) ([]runctl.Orphan, error) {
		lists.Add(1)
		return []runctl.Orphan{orphan}, nil
	}

	runctl.StartRecovery(context.Background(), list, &expiresAfterFirstCheck{}, q, 20*time.Millisecond)

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
	list := func(context.Context) ([]runctl.Orphan, error) {
		lists.Add(1)
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	runctl.StartRecovery(ctx, list, &fakeLiveness{}, &recordingQueue{}, 30*time.Millisecond)
	cancel() // shutdown before the recheck is due

	time.Sleep(120 * time.Millisecond)
	if n := lists.Load(); n != 1 {
		t.Errorf("listed %d times, want 1: the recheck ran after the context was cancelled", n)
	}
}

// An orphan names the job that recovers it: a session still marked running is a
// resume, and an approval decided but never resumed is an approval job carrying
// its id.
// fakeThrottle allows each session key a set number of times, then refuses; the keys
// in broken return an error.
type fakeThrottle struct {
	mu     sync.Mutex
	allow  int
	used   map[string]int
	broken map[string]bool
	asked  []string
}

func (f *fakeThrottle) Allow(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, key)
	if f.broken[key] {
		return false, errors.New("redis: timeout")
	}
	if f.used == nil {
		f.used = map[string]int{}
	}
	if f.used[key] >= f.allow {
		return false, nil
	}
	f.used[key]++
	return true, nil
}

// A recurring sweep must not pile a job on a session every pass: with a throttle
// that allows a session once, two sweeps over the same orphan enqueue one job.
func TestRecoverOrphans_AThrottleKeepsARecurringSweepFromReEnqueueingTheSameSession(t *testing.T) {
	q := &recordingQueue{}
	orphan := candidate("a")
	throttle := &fakeThrottle{allow: 1}

	first := runctl.RecoverOrphans(context.Background(), "periodic", []runctl.Orphan{orphan}, &fakeLiveness{}, q, throttle)
	second := runctl.RecoverOrphans(context.Background(), "periodic", []runctl.Orphan{orphan}, &fakeLiveness{}, q, throttle)

	if first != 1 || second != 0 || len(q.enqueuedSessions()) != 1 {
		t.Fatalf("first=%d second=%d enqueued=%d, want 1, 0 and one job in all", first, second, len(q.enqueuedSessions()))
	}
}

// nil is today's behaviour: every sweep enqueues.
func TestRecoverOrphans_ANilThrottleEnqueuesEveryTime(t *testing.T) {
	q := &recordingQueue{}
	orphan := candidate("a")
	runctl.RecoverOrphans(context.Background(), "startup", []runctl.Orphan{orphan}, &fakeLiveness{}, q, nil)
	runctl.RecoverOrphans(context.Background(), "recheck", []runctl.Orphan{orphan}, &fakeLiveness{}, q, nil)
	if got := len(q.enqueuedSessions()); got != 2 {
		t.Fatalf("%d jobs enqueued by two unthrottled sweeps, want 2", got)
	}
}

// A held lock is checked first and never spends the throttle: the session is being
// driven, and is not one recovery re-enqueued.
func TestRecoverOrphans_ALiveSessionDoesNotSpendTheThrottle(t *testing.T) {
	q := &recordingQueue{}
	live := candidate("live")
	throttle := &fakeThrottle{allow: 1}

	runctl.RecoverOrphans(context.Background(), "periodic", []runctl.Orphan{live}, &fakeLiveness{held: map[string]bool{"live": true}}, q, throttle)
	if len(throttle.asked) != 0 {
		t.Fatalf("the throttle was asked about a live session: %v", throttle.asked)
	}
	// Once its lock is gone the session is recovered, because the throttle was never used up.
	if got := runctl.RecoverOrphans(context.Background(), "periodic", []runctl.Orphan{live}, &fakeLiveness{}, q, throttle); got != 1 {
		t.Fatalf("recovered %d once the lock was gone, want 1", got)
	}
}

// A throttle that cannot answer leaves the session for the next pass, as an unreadable
// lock does: unknown is not permission.
func TestRecoverOrphans_AThrottleThatCannotAnswerLeavesTheSession(t *testing.T) {
	q := &recordingQueue{}
	a, b := candidate("a"), candidate("b")
	throttle := &fakeThrottle{allow: 1, broken: map[string]bool{"a": true}}

	if got := runctl.RecoverOrphans(context.Background(), "periodic", []runctl.Orphan{a, b}, &fakeLiveness{}, q, throttle); got != 1 {
		t.Fatalf("recovered %d, want only the session the throttle could answer for", got)
	}
	if ids := q.enqueuedSessions(); len(ids) != 1 || ids[0] != b.SessionID {
		t.Fatalf("enqueued %v, want only b", ids)
	}
}

func TestRecoverOrphans_EnqueuesTheKindAndPayloadTheOrphanNames(t *testing.T) {
	q := &recordingQueue{}
	plain := candidate("a")
	decided := candidate("b")
	decided.Kind, decided.Payload = queue.KindApproval, json.RawMessage(`{"approval_id":"x"}`)

	runctl.RecoverOrphans(context.Background(), "test", []runctl.Orphan{plain, decided}, &fakeLiveness{}, q, nil)

	if len(q.jobs) != 2 {
		t.Fatalf("%d jobs enqueued, want 2", len(q.jobs))
	}
	if q.jobs[0].Kind != queue.KindResume || q.jobs[0].Payload != nil {
		t.Errorf("a plain orphan = %+v, want a resume with no payload", q.jobs[0])
	}
	if q.jobs[1].Kind != queue.KindApproval || string(q.jobs[1].Payload) != `{"approval_id":"x"}` {
		t.Errorf("a decided approval = %+v, want an approval job with its payload", q.jobs[1])
	}
}
