package queue

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent/internal/reliability"
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
func (fakeLock) Renew(context.Context, string, string) (bool, error)   { return true, nil }

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
	for range 3 {
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

// renewLock is a Locker whose Renew is scripted: each call is passed to renew,
// and the calls are counted.
type renewLock struct {
	renewals atomic.Int32
	renew    func(call int32) (bool, error)
}

func (*renewLock) Acquire(context.Context, string) (string, bool, error) { return "tok", true, nil }
func (*renewLock) Release(context.Context, string, string) error         { return nil }
func (l *renewLock) Renew(context.Context, string, string) (bool, error) {
	return l.renew(l.renewals.Add(1))
}

func newRenewWorker(port *fakePort, lock Locker, run func(context.Context, Job) error) *Worker {
	return NewWorker(WorkerConfig{
		Port: port, Lock: lock, Runner: funcRunner(run), Owner: "w1",
		PollEvery: time.Millisecond, RenewEvery: 5 * time.Millisecond,
	})
}

// The lock's TTL is shorter than a turn loop can run, so a worker must keep
// renewing it for as long as the job runs, and stop once it is done.
func TestWorker_RenewsTheSessionLockWhileAJobRuns(t *testing.T) {
	port := newFakePort()
	job, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "s1", Kind: KindResume})
	lock := &renewLock{renew: func(int32) (bool, error) { return true, nil }}

	newRenewWorker(port, lock, func(ctx context.Context, _ Job) error {
		select {
		case <-time.After(80 * time.Millisecond): // many renew intervals
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}).pollOnce(context.Background())

	during := lock.renewals.Load()
	if during < 3 {
		t.Errorf("renewed %d times during an 80ms run with a 5ms interval, want several", during)
	}
	time.Sleep(30 * time.Millisecond)
	if after := lock.renewals.Load(); after != during {
		t.Errorf("renewals went from %d to %d after the job finished: the heartbeat outlived the run", during, after)
	}
	if got := port.status(job.JobID); got != StatusDone {
		t.Errorf("job status = %q, want done", got)
	}
}

// A run that no longer holds its session must stop rather than race whoever
// does, and must not be reported as a success.
func TestWorker_CancelsARunThatLostItsLock(t *testing.T) {
	port := newFakePort()
	job, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "s1", Kind: KindResume})
	lock := &renewLock{renew: func(int32) (bool, error) { return false, nil }}

	var cancelled bool
	newRenewWorker(port, lock, func(ctx context.Context, _ Job) error {
		select {
		case <-ctx.Done():
			cancelled = true
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return nil // only reached if the run was never cancelled
		}
	}).pollOnce(context.Background())

	if !cancelled {
		t.Fatal("the run was not cancelled after its lock was lost")
	}
	if got := port.status(job.JobID); got == StatusDone {
		t.Errorf("job status = %q: a run that lost its lock was completed", got)
	}
}

// A failed renewal (Redis briefly unreachable) is not a lost lock: it lives
// until its TTL, so the run continues and the next tick tries again.
func TestWorker_ATransientRenewErrorDoesNotCancelTheRun(t *testing.T) {
	port := newFakePort()
	job, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "s1", Kind: KindResume})
	lock := &renewLock{renew: func(call int32) (bool, error) {
		if call <= 2 {
			return false, errors.New("redis: connection refused")
		}
		return true, nil
	}}

	var cancelled bool
	newRenewWorker(port, lock, func(ctx context.Context, _ Job) error {
		select {
		case <-time.After(60 * time.Millisecond):
			return nil
		case <-ctx.Done():
			cancelled = true
			return ctx.Err()
		}
	}).pollOnce(context.Background())

	if cancelled {
		t.Error("a transient renewal error cancelled the run")
	}
	if got := port.status(job.JobID); got != StatusDone {
		t.Errorf("job status = %q, want done", got)
	}
}

// extendingPort is a Port that also implements LeaseExtender, counting the calls.
type extendingPort struct {
	*fakePort
	extends atomic.Int32
	err     error
	jobs    sync.Map // job ids Extend was called for
}

func (p *extendingPort) Extend(_ context.Context, jobID uuid.UUID) error {
	p.extends.Add(1)
	p.jobs.Store(jobID, true)
	return p.err
}

// Lease reclaims an entry that has been idle for LeaseFor, and nothing else
// refreshes it while a job runs, so a run longer than the lease was handed to a
// second worker. The worker has to tell the queue the job is still being worked.
func TestWorker_ExtendsTheLeaseWhileAJobRuns(t *testing.T) {
	port := &extendingPort{fakePort: newFakePort()}
	job, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "s1", Kind: KindResume})
	lock := &renewLock{renew: func(int32) (bool, error) { return true, nil }}

	w := NewWorker(WorkerConfig{
		Port: port, Lock: lock, Owner: "w1", PollEvery: time.Millisecond, RenewEvery: 10 * time.Millisecond,
		Runner: funcRunner(func(ctx context.Context, _ Job) error {
			select {
			case <-time.After(45 * time.Millisecond): // more than three renew intervals
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}),
	})
	w.pollOnce(context.Background())

	during := port.extends.Load()
	if during < 2 {
		t.Errorf("Extend was called %d times during a run of several renew intervals, want at least 2", during)
	}
	if _, ok := port.jobs.Load(job.JobID); !ok {
		t.Error("Extend was not called for the job being run")
	}
	time.Sleep(30 * time.Millisecond)
	if after := port.extends.Load(); after != during {
		t.Errorf("Extend calls went from %d to %d after the job finished: the heartbeat outlived the run", during, after)
	}
	if got := port.status(job.JobID); got != StatusDone {
		t.Errorf("job status = %q, want done", got)
	}
}

// A failed extension (Redis briefly unreachable) must not end the run: the next tick tries again.
func TestWorker_AFailedLeaseExtensionDoesNotCancelTheRun(t *testing.T) {
	port := &extendingPort{fakePort: newFakePort(), err: errors.New("redis: connection refused")}
	job, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "s1", Kind: KindResume})
	lock := &renewLock{renew: func(int32) (bool, error) { return true, nil }}

	var cancelled bool
	NewWorker(WorkerConfig{
		Port: port, Lock: lock, Owner: "w1", PollEvery: time.Millisecond, RenewEvery: 5 * time.Millisecond,
		Runner: funcRunner(func(ctx context.Context, _ Job) error {
			select {
			case <-time.After(40 * time.Millisecond):
				return nil
			case <-ctx.Done():
				cancelled = true
				return ctx.Err()
			}
		}),
	}).pollOnce(context.Background())

	if cancelled || port.extends.Load() < 2 {
		t.Errorf("cancelled=%v after %d extension attempts: a failed extension must be retried, not fatal", cancelled, port.extends.Load())
	}
	if got := port.status(job.JobID); got != StatusDone {
		t.Errorf("job status = %q, want done", got)
	}
}

// A Port with no LeaseExtender is every existing adapter and test: the worker still runs the job.
func TestWorker_APortWithoutLeaseExtenderStillWorks(t *testing.T) {
	var port Port = newFakePort()
	if _, ok := port.(LeaseExtender); ok {
		t.Fatal("fixture: fakePort must not implement LeaseExtender")
	}
	fp := port.(*fakePort)
	job, _ := fp.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "s1", Kind: KindResume})
	lock := &renewLock{renew: func(int32) (bool, error) { return true, nil }}

	newRenewWorker(fp, lock, func(ctx context.Context, _ Job) error {
		select {
		case <-time.After(30 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}).pollOnce(context.Background())

	if got := fp.status(job.JobID); got != StatusDone {
		t.Fatalf("job status = %q, want done", got)
	}
}

// One registry is shared, and a session that runs fine drops out of it.
func TestWorker_ASuccessfulJobForgetsItsSessionsBreaker(t *testing.T) {
	port := newFakePort()
	breakers := reliability.NewBreakerRegistry(3)

	fail := true
	w := NewWorker(WorkerConfig{
		Port: port, Lock: newTestLock(t), Breakers: breakers, Owner: "w1",
		Backoff: reliability.BackoffConfig{Base: time.Microsecond, Max: time.Millisecond},
		Runner: funcRunner(func(context.Context, Job) error {
			if fail {
				return errors.New("transient")
			}
			return nil
		}),
	})
	enqueue := func() uuid.UUID {
		j, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "s1", Kind: KindResume})
		return j.JobID
	}

	enqueue()
	w.pollOnce(context.Background())
	if breakers.Len() != 1 {
		t.Fatalf("registry tracks %d keys after a failure, want 1", breakers.Len())
	}

	fail = false
	time.Sleep(5 * time.Millisecond) // past the retry's backoff
	w.pollOnce(context.Background())
	if breakers.Len() != 0 {
		t.Fatalf("registry still tracks %d keys after the session succeeded", breakers.Len())
	}
}

// ctxStrictPort fails any Complete or Fail called with a context that has ended, as a
// real Redis call would, and records what it settled.
type ctxStrictPort struct {
	*fakePort
	mu       sync.Mutex
	rejected int
}

func (p *ctxStrictPort) Complete(ctx context.Context, id uuid.UUID) error {
	if ctx.Err() != nil {
		p.mu.Lock()
		p.rejected++
		p.mu.Unlock()
		return ctx.Err()
	}
	return p.fakePort.Complete(ctx, id)
}

func (p *ctxStrictPort) Fail(ctx context.Context, id uuid.UUID, reason string, permanent bool, retryAt time.Time) error {
	if ctx.Err() != nil {
		p.mu.Lock()
		p.rejected++
		p.mu.Unlock()
		return ctx.Err()
	}
	return p.fakePort.Fail(ctx, id, reason, permanent, retryAt)
}

func (p *ctxStrictPort) rejectedCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rejected
}

// A shutdown stops leasing but must not cut the runs under way: Principle VIII says a
// deploy does not cut a running agent over mid-task.
func TestWorker_ARunSurvivesTheLeaseContextBeingCancelledAndFinishes(t *testing.T) {
	port := &ctxStrictPort{fakePort: newFakePort()}
	job, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "s1", Kind: KindResume})

	jobCtx, cancelJobs := context.WithCancel(t.Context())
	defer cancelJobs()
	leaseCtx, cancelLease := context.WithCancel(t.Context())
	defer cancelLease()

	started := make(chan struct{})
	release := make(chan struct{})
	var runCtxEnded atomic.Bool
	w := NewWorker(WorkerConfig{
		Port: port, Lock: newTestLock(t), Owner: "w1", PollEvery: time.Millisecond, JobContext: jobCtx,
		Runner: funcRunner(func(ctx context.Context, _ Job) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				runCtxEnded.Store(true)
				return ctx.Err()
			}
		}),
	})
	stopped := make(chan struct{})
	go func() { defer close(stopped); w.Run(leaseCtx) }()

	<-started
	cancelLease() // SIGTERM: stop leasing
	time.Sleep(50 * time.Millisecond)
	if runCtxEnded.Load() {
		t.Fatal("cancelling the lease context cancelled the run in progress")
	}
	select {
	case <-stopped:
		t.Fatal("the worker returned while its job was still running")
	default:
	}

	close(release) // the run finishes within the grace period
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker did not return after its job finished")
	}
	if got := port.status(job.JobID); got != StatusDone {
		t.Fatalf("job status = %q, want done", got)
	}
}

// When the grace period runs out the job context is cancelled and the run ends. The job
// still has to go back on the queue and its lock be released, which a cancelled context
// would make impossible.
func TestWorker_ACancelledJobContextEndsTheRunAndStillFailsTheJobBack(t *testing.T) {
	port := &ctxStrictPort{fakePort: newFakePort()}
	job, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "s1", Kind: KindResume})
	released := make(chan struct{}, 1)
	lock := &releaseLock{released: released}

	jobCtx, cancelJobs := context.WithCancel(t.Context())
	defer cancelJobs()
	started := make(chan struct{})
	w := NewWorker(WorkerConfig{
		Port: port, Lock: lock, Owner: "w1", PollEvery: time.Millisecond, JobContext: jobCtx,
		Backoff: reliability.BackoffConfig{Base: time.Millisecond, Max: 5 * time.Millisecond},
		Runner: funcRunner(func(ctx context.Context, _ Job) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}),
	})
	leaseCtx, cancelLease := context.WithCancel(t.Context())
	defer cancelLease()
	stopped := make(chan struct{})
	go func() { defer close(stopped); w.Run(leaseCtx) }()

	<-started
	cancelLease()
	cancelJobs() // the grace period ran out
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker did not return after its job context was cancelled")
	}

	if got := port.status(job.JobID); got != StatusPending {
		t.Fatalf("job status = %q, want pending: a cancelled run is failed back for another worker to resume", got)
	}
	if port.rejectedCalls() != 0 {
		t.Fatalf("%d Complete/Fail calls were made with an ended context", port.rejectedCalls())
	}
	select {
	case <-released:
	default:
		t.Fatal("the session lock was not released")
	}
}

// releaseLock is a Locker that signals when Release is called, with the context it got.
type releaseLock struct {
	fakeLock
	released chan struct{}
}

func (l *releaseLock) Release(ctx context.Context, _, _ string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	select {
	case l.released <- struct{}{}:
	default:
	}
	return nil
}

// A panic in the kernel, a provider adapter or run control would end the whole process
// and every other run with it. It fails that one job, for good, and the worker carries on.
func TestWorker_APanickingJobIsFailedPermanentlyAndTheWorkerLeasesTheNextOne(t *testing.T) {
	port := newFakePort()
	bad, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "bad", Kind: KindResume})
	good, _ := port.Enqueue(context.Background(), Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: "good", Kind: KindResume})

	released := make(chan struct{}, 4)
	w := NewWorker(WorkerConfig{
		Port: port, Lock: &releaseLock{released: released}, Owner: "w1", PollEvery: time.Millisecond,
		Admission: NewAdmissionController(1), // a leaked slot would starve the second job
		Runner: funcRunner(func(_ context.Context, j Job) error {
			if j.SessionKey == "bad" {
				panic("a kernel invariant was violated")
			}
			return nil
		}),
	})

	w.pollOnce(context.Background())
	if got := port.status(bad.JobID); got != StatusFailed {
		t.Fatalf("the panicking job's status = %q, want failed (permanent): retrying a panic crash-loops a worker", got)
	}
	port.mu.Lock()
	lastError := port.jobs[bad.JobID].LastError
	port.mu.Unlock()
	if !strings.Contains(lastError, "job panicked") {
		t.Fatalf("last error = %q, want it to say the job panicked", lastError)
	}
	if len(released) != 1 {
		t.Fatal("the panicking job's session lock was not released")
	}

	w.pollOnce(context.Background())
	if got := port.status(good.JobID); got != StatusDone {
		t.Fatalf("the next job's status = %q, want done: the worker must survive a panic and keep its admission slot free", got)
	}
}

func TestWorker_APanicIsAPermanentFailureWhatEverItCarries(t *testing.T) {
	for name, v := range map[string]any{"an error": errors.New("boom"), "a string": "boom", "an int": 7} {
		w := NewWorker(WorkerConfig{Runner: funcRunner(func(context.Context, Job) error { panic(v) })})
		err := w.runRecovered(context.Background(), Job{})
		if reliability.Classify(err) != reliability.FailurePermanent || !strings.Contains(err.Error(), "job panicked") {
			t.Errorf("%s: err = %v, want a permanent \"job panicked\" failure", name, err)
		}
	}
}
