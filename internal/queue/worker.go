package queue

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/rs/zerolog/log"

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
	// Renew extends a held lock's TTL; false means the lock is no longer this
	// token's (it expired or was taken).
	Renew(ctx context.Context, sessionKey, token string) (bool, error)
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
	// RenewEvery is how often a running job renews its session lock. It must be
	// well under the lock's TTL (cmd/nexusd configures 30s); the default is 10s.
	RenewEvery time.Duration
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
	if c.RenewEvery <= 0 {
		c.RenewEvery = 10 * time.Second
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
		log.Error().Err(err).Msg("queue: lease failed")
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
		log.Error().Err(err).Str("session_key", job.SessionKey).Msg("queue: session lock acquire failed")
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
			log.Error().Err(err).Str("session_key", job.SessionKey).Msg("queue: session lock release failed")
		}
	}()

	// The lock's TTL is far shorter than a turn loop can run, so it is renewed
	// for as long as the job runs, and the run is cancelled if it is ever lost.
	runCtx, cancelRun := context.WithCancel(ctx)
	stopRenewing := w.holdLock(runCtx, cancelRun, job.SessionKey, token)
	runErr := w.cfg.Runner.Run(runCtx, job)
	stopRenewing()
	cancelRun()
	if runErr == nil {
		breaker.RecordSuccess()
		if err := w.cfg.Port.Complete(ctx, job.JobID); err != nil {
			log.Error().Err(err).Any("job_id", job.JobID).Msg("queue: complete failed")
		}
		return
	}

	class := reliability.Classify(runErr)
	tripped := breaker.RecordFailure(runErr.Error())
	permanent := class == reliability.FailurePermanent || tripped
	delay := w.cfg.Backoff.Delay(job.Attempts, w.rng)
	log.Warn().Err(runErr).Any("job_id", job.JobID).Str("session_key", job.SessionKey).
		Any("class", class).Bool("breaker_tripped", tripped).Any("backoff_reason", delay.Reason).
		Msg("queue: job failed")
	w.failJob(ctx, job, runErr.Error(), permanent, time.Now().Add(delay.Duration))
}

func (w *Worker) failJob(ctx context.Context, job Job, reason string, permanent bool, retryAt time.Time) {
	if err := w.cfg.Port.Fail(ctx, job.JobID, reason, permanent, retryAt); err != nil {
		log.Error().Err(err).Any("job_id", job.JobID).Msg("queue: fail failed")
	}
}

// holdLock renews the session lock every RenewEvery until the returned stop is
// called. Without it the lock lapsed after its TTL while the turn loop was
// still running, "per-session serial" stopped holding for any run longer than
// that, and a second job for the same session (a follow-up message, a reclaimed
// lease) could be driven concurrently. If the lock turns out to be lost, the
// run is cancelled: a run that no longer holds its session must stop rather than
// race whoever does. A failed renewal (Redis briefly unreachable) is not a lost
// lock: it still lives until its TTL, so the next tick tries again.
func (w *Worker) holdLock(ctx context.Context, cancelRun context.CancelFunc, sessionKey, token string) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(w.cfg.RenewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				held, err := w.cfg.Lock.Renew(ctx, sessionKey, token)
				if err != nil {
					log.Warn().Err(err).Str("session_key", sessionKey).Msg("queue: session lock renewal failed; will retry")
					continue
				}
				if !held {
					log.Error().Str("session_key", sessionKey).Msg("queue: session lock lost mid-run; cancelling the run so it cannot race the new holder")
					cancelRun()
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}
