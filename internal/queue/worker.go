package queue

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/truongpx396/nexus-agent-demo/internal/reliability"
)

// Runner is what a worker calls to actually DO one leased job —
// internal/runctl's Resume/Fork/Steer operations, wrapped by whatever
// composition root (cmd/nexusd) builds them. This package stays free of a
// kernel/tools/store dependency the same decoupling idiom as
// kernel.ToolExecutor: it only knows "a session-scoped unit of work either
// succeeds or fails," never what that work actually is. A Runner that wants
// a failure treated as non-retryable wraps it in
// *reliability.PermanentError; anything else is classified by
// reliability.Classify.
type Runner interface {
	Run(ctx context.Context, job Job) error
}

// Locker is the small structural interface *SessionLock satisfies — Worker
// depends on this, not *SessionLock directly, so a unit test can swap in an
// always-succeeds fake instead of standing up real Redis (tests/integration
// is where SessionLock's own Lua atomicity gets exercised against the real
// thing, matching internal/cost.Gate's scripter/redisScripter split).
type Locker interface {
	Acquire(ctx context.Context, sessionKey string) (token string, ok bool, err error)
	Release(ctx context.Context, sessionKey, token string) error
}

// WorkerConfig is one Worker's construction-time tuning.
type WorkerConfig struct {
	Port      Port
	Lock      Locker
	Runner    Runner
	Admission *AdmissionController
	Breakers  *reliability.BreakerRegistry
	Backoff   reliability.BackoffConfig

	Owner     string        // this worker's identity, recorded as lease_owner and used to derive the lock token's namespace
	LeaseFor  time.Duration // how long a lease is held before it's considered abandoned
	PollEvery time.Duration // how often to poll when nothing is leasable
}

func (c WorkerConfig) withDefaults() WorkerConfig {
	if c.Admission == nil {
		c.Admission = NewAdmissionController(4)
	}
	if c.Breakers == nil {
		c.Breakers = reliability.NewBreakerRegistry(3)
	}
	if c.LeaseFor <= 0 {
		c.LeaseFor = 2 * time.Minute
	}
	if c.PollEvery <= 0 {
		c.PollEvery = 500 * time.Millisecond
	}
	return c
}

// Worker is the pool README task 6.1 names: poll -> lease -> acquire the
// session-key serial lock (task 6.2) -> run -> classify the outcome (task
// 6.7) -> Complete, or Fail with a logged, jittered backoff and a
// per-session-key circuit breaker -> release the lock -> repeat. Run blocks
// until ctx is done; call it in its own goroutine (cmd/nexusd's serve()
// starts one or more, mirroring startAnchorLoop's own background-loop
// idiom).
type Worker struct {
	cfg WorkerConfig
	rng *rand.Rand
}

func NewWorker(cfg WorkerConfig) *Worker {
	return &Worker{cfg: cfg.withDefaults(), rng: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0))} //nolint:gosec // poll/backoff jitter, not a security decision
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.PollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollOnce(ctx)
		}
	}
}

func (w *Worker) pollOnce(ctx context.Context) {
	job, ok, err := w.cfg.Port.Lease(ctx, w.cfg.Owner, w.cfg.LeaseFor)
	if err != nil {
		slog.Error("queue: lease failed", "error", err)
		return
	}
	if !ok {
		return
	}

	breaker := w.cfg.Breakers.For(job.SessionKey)
	if breaker.Open() {
		// A previous run of identical failures already tripped this
		// session's breaker — refuse to burn an admission slot retrying it;
		// requeue far enough out that a human has a real chance to
		// intervene (internal/runctl.ResolveClaim, a cancel, ...) before it
		// comes back around.
		w.failJob(ctx, job, "circuit_breaker_open: refusing to retry until an operator intervenes", false, time.Now().Add(5*time.Minute))
		return
	}

	release, err := w.cfg.Admission.Acquire(ctx)
	if err != nil {
		// ctx was canceled while waiting for a slot — put the job straight
		// back rather than leaving it leased-but-abandoned until the lease
		// itself expires.
		w.failJob(ctx, job, "admission: "+err.Error(), false, time.Now())
		return
	}
	defer release()

	token, locked, err := w.cfg.Lock.Acquire(ctx, job.SessionKey)
	if err != nil {
		slog.Error("queue: session lock acquire failed", "session_key", job.SessionKey, "error", err)
		w.failJob(ctx, job, "session_lock_error: "+err.Error(), false, time.Now().Add(w.cfg.Backoff.Delay(job.Attempts, w.rng).Duration))
		return
	}
	if !locked {
		// Another worker already holds this session's serial slot — put the
		// job back for a quick retry rather than blocking this worker on it
		// (task 6.2: cross-session concurrent, per-session serial).
		w.failJob(ctx, job, "session_lock_held: another worker is already processing this session", false, time.Now().Add(200*time.Millisecond))
		return
	}
	defer func() {
		if err := w.cfg.Lock.Release(ctx, job.SessionKey, token); err != nil {
			slog.Error("queue: session lock release failed", "session_key", job.SessionKey, "error", err)
		}
	}()

	runErr := w.cfg.Runner.Run(ctx, job)
	if runErr == nil {
		breaker.RecordSuccess()
		if err := w.cfg.Port.Complete(ctx, job.JobID); err != nil {
			slog.Error("queue: complete failed", "job_id", job.JobID, "error", err)
		}
		return
	}

	class := reliability.Classify(runErr)
	tripped := breaker.RecordFailure(runErr.Error())
	permanent := class == reliability.FailurePermanent || tripped
	delay := w.cfg.Backoff.Delay(job.Attempts, w.rng)
	slog.Warn("queue: job failed", "job_id", job.JobID, "session_key", job.SessionKey,
		"class", class, "breaker_tripped", tripped, "error", runErr, "backoff_reason", delay.Reason)
	w.failJob(ctx, job, runErr.Error(), permanent, time.Now().Add(delay.Duration))
}

func (w *Worker) failJob(ctx context.Context, job Job, reason string, permanent bool, retryAt time.Time) {
	if err := w.cfg.Port.Fail(ctx, job.JobID, reason, permanent, retryAt); err != nil {
		slog.Error("queue: fail failed", "job_id", job.JobID, "error", err)
	}
}
