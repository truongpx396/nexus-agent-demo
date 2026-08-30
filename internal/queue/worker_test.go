package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/reliability"
)

// fakePort is an in-memory Port for worker tests — no Postgres dependency,
// the same reason internal/cost's Gate tests use a fake scripter instead of
// real Redis for anything that doesn't specifically need SKIP LOCKED's own
// atomicity (that's what tests/integration exercises for real).
type fakePort struct {
	mu    sync.Mutex
	jobs  map[uuid.UUID]*Job
	order []uuid.UUID
}

func newFakePort() *fakePort { return &fakePort{jobs: map[uuid.UUID]*Job{}} }

func (p *fakePort) Enqueue(_ context.Context, job Job) (Job, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if job.JobID == uuid.Nil {
		job.JobID = uuid.New()
	}
	job.Status = StatusPending
	if job.AvailableAt.IsZero() {
		job.AvailableAt = time.Now()
	}
	p.jobs[job.JobID] = &job
	p.order = append(p.order, job.JobID)
	return job, nil
}

func (p *fakePort) Lease(_ context.Context, owner string, leaseFor time.Duration) (Job, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, id := range p.order {
		j := p.jobs[id]
		if j.Status == StatusPending && !j.AvailableAt.After(now) {
			j.Status = StatusLeased
			j.Attempts++
			j.LeaseOwner = owner
			exp := now.Add(leaseFor)
			j.LeaseExpiresAt = &exp
			return *j, true, nil
		}
	}
	return Job{}, false, nil
}

func (p *fakePort) Complete(_ context.Context, jobID uuid.UUID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	j, ok := p.jobs[jobID]
	if !ok {
		return errors.New("no such job")
	}
	j.Status = StatusDone
	return nil
}

func (p *fakePort) Fail(_ context.Context, jobID uuid.UUID, reason string, permanent bool, retryAt time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	j, ok := p.jobs[jobID]
	if !ok {
		return errors.New("no such job")
	}
	j.LastError = reason
	j.AvailableAt = retryAt
	if permanent {
		j.Status = StatusFailed
	} else {
		j.Status = StatusPending
	}
	return nil
}

func (p *fakePort) status(id uuid.UUID) Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.jobs[id].Status
}

// funcRunner adapts a plain func to Runner.
type funcRunner func(ctx context.Context, job Job) error

func (f funcRunner) Run(ctx context.Context, job Job) error { return f(ctx, job) }

// fakeLock is a Locker stand-in that always succeeds — worker_test.go
// exercises Worker's own poll/lease/run/classify logic, not
// SessionLock/Postgres's real atomicity (tests/integration covers that
// against real Redis/Postgres, matching internal/cost's own precedent for
// what needs a live backend vs. a fake).
type fakeLock struct{}

func (fakeLock) Acquire(context.Context, string) (string, bool, error) { return "tok", true, nil }
func (fakeLock) Release(context.Context, string, string) error         { return nil }

func TestWorker_RunsLeasedJobAndCompletesOnSuccess(t *testing.T) {
	port := newFakePort()
	job, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "s1", Kind: KindResume})

	var ran bool
	w := NewWorker(WorkerConfig{
		Port: port, Lock: newTestLock(t), Runner: funcRunner(func(_ context.Context, j Job) error {
			ran = true
			if j.JobID != job.JobID {
				t.Fatalf("Runner got job %s, want %s", j.JobID, job.JobID)
			}
			return nil
		}),
		Owner: "w1", PollEvery: time.Millisecond,
	})
	w.pollOnce(context.Background())

	if !ran {
		t.Fatal("Runner.Run was never called")
	}
	if got := port.status(job.JobID); got != StatusDone {
		t.Fatalf("job status = %q, want done", got)
	}
}

func TestWorker_RetryableFailureRequeuesWithBackoff(t *testing.T) {
	port := newFakePort()
	job, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "s1", Kind: KindResume})

	w := NewWorker(WorkerConfig{
		Port: port, Lock: newTestLock(t),
		Runner:  funcRunner(func(context.Context, Job) error { return errors.New("transient") }),
		Owner:   "w1",
		Backoff: reliability.BackoffConfig{Base: time.Millisecond, Max: time.Second},
	})
	before := time.Now()
	w.pollOnce(context.Background())

	if got := port.status(job.JobID); got != StatusPending {
		t.Fatalf("job status = %q, want pending (requeued for retry)", got)
	}
	port.mu.Lock()
	availableAt := port.jobs[job.JobID].AvailableAt
	port.mu.Unlock()
	if !availableAt.After(before) {
		t.Fatalf("available_at = %s, want pushed into the future by backoff", availableAt)
	}
}

func TestWorker_PermanentFailureFailsWithoutRetry(t *testing.T) {
	port := newFakePort()
	job, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "s1", Kind: KindResume})

	w := NewWorker(WorkerConfig{
		Port: port, Lock: newTestLock(t),
		Runner: funcRunner(func(context.Context, Job) error {
			return &reliability.PermanentError{Err: errors.New("session erased")}
		}),
		Owner: "w1",
	})
	w.pollOnce(context.Background())

	if got := port.status(job.JobID); got != StatusFailed {
		t.Fatalf("job status = %q, want failed", got)
	}
}

func TestWorker_CircuitBreakerOpensAfterRepeatedIdenticalFailures(t *testing.T) {
	port := newFakePort()
	sessionKey := "flaky-session"
	breakers := reliability.NewBreakerRegistry(3)
	w := NewWorker(WorkerConfig{
		Port: port, Lock: newTestLock(t), Breakers: breakers,
		Runner:  funcRunner(func(context.Context, Job) error { return errors.New("same reason every time") }),
		Owner:   "w1",
		Backoff: reliability.BackoffConfig{Base: time.Microsecond, Max: time.Millisecond},
	})

	// Three identical failures trip the breaker (internal/reliability's own
	// threshold-3 default and this test's explicit registry).
	for i := 0; i < 3; i++ {
		job, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: sessionKey, Kind: KindResume, AvailableAt: time.Now()})
		w.pollOnce(context.Background())
		_ = job
	}
	if !breakers.For(sessionKey).Open() {
		t.Fatal("breaker should be open after 3 identical failures")
	}

	// A 4th job for the SAME session must be refused WITHOUT the Runner
	// running at all — the whole point of the breaker.
	ranAgain := false
	w2 := NewWorker(WorkerConfig{
		Port: port, Lock: newTestLock(t), Breakers: breakers,
		Runner: funcRunner(func(context.Context, Job) error { ranAgain = true; return nil }),
		Owner:  "w1",
	})
	job, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: sessionKey, Kind: KindResume, AvailableAt: time.Now()})
	w2.pollOnce(context.Background())
	if ranAgain {
		t.Fatal("Runner must not run once the breaker for this session_key is open")
	}
	if got := port.status(job.JobID); got != StatusPending {
		t.Fatalf("job status = %q, want pending (deferred, not failed permanently, while the breaker is open)", got)
	}
}

func TestWorker_NothingLeasableIsANoop(t *testing.T) {
	port := newFakePort()
	w := NewWorker(WorkerConfig{
		Port: port, Lock: newTestLock(t),
		Runner: funcRunner(func(context.Context, Job) error {
			t.Fatal("Runner must not be called with nothing leasable")
			return nil
		}),
		Owner: "w1",
	})
	w.pollOnce(context.Background())
}

func newTestLock(t *testing.T) Locker {
	t.Helper()
	return fakeLock{}
}
