// Package reliability implements constitution Principle VIII — "classify,
// resume, never silently retry" — for the two places this codebase needs it
// beyond internal/provider's own narrower TriggerClass taxonomy (README
// task 2.9, which classifies only a Provider.Stream failure): a queue job
// that failed (internal/queue's worker, task 6.7) and a run that looks
// stuck (task 6.8). Both stay free of any store/crypto/kernel dependency —
// pure, in-memory, and cheap to unit test — exactly like internal/provider's
// own ClassifyTrigger/Wrap.
package reliability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"time"
)

// FailureClass is the typed taxonomy a worker-level failure is classified
// into BEFORE any retry (task 6.7's own wording) — distinct from
// internal/provider.TriggerClass, which is scoped to a Provider.Stream
// failure specifically and is classified INSIDE the kernel loop, not at the
// worker/job level this package's callers (internal/queue) operate at.
type FailureClass string

const (
	// FailureRetryable covers a timeout, a dropped connection, or anything
	// else another attempt might not hit.
	FailureRetryable FailureClass = "retryable"
	// FailurePermanent covers everything else — retrying deterministically
	// reproduces the same failure, so a circuit breaker should trip on it
	// fast rather than burn through backoff attempts first.
	FailurePermanent FailureClass = "permanent"
)

// Classify classifies an error a queue job's Runner returned. Unrecognized
// errors fail toward FailureRetryable — the same "give it one more honest
// chance" bias internal/provider.ClassifyTrigger's own default (Permanent)
// deliberately does NOT take, because that package's failures are usually
// well-typed provider errors; a queue Runner's errors are much more likely
// to be an ordinary transient I/O problem this package has never seen
// before, so defaulting to Permanent here would trip the breaker on
// failures a plain retry would have cleared.
func Classify(err error) FailureClass {
	if err == nil {
		return FailureRetryable
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, io.ErrUnexpectedEOF) {
		return FailureRetryable
	}
	if _, ok := errors.AsType[*PermanentError](err); ok {
		return FailurePermanent
	}
	return FailureRetryable
}

// PermanentError lets a Runner mark its own failure as definitely not worth
// retrying (e.g., a job whose session no longer exists) — Classify honors
// it explicitly rather than trying to pattern-match every possible
// permanent condition a future Runner might invent.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// BackoffConfig computes the delay before a retryable failure's next
// attempt: exponential with full jitter (Base * 2^attempt, capped at Max,
// then uniformly randomized down from that cap) — the standard shape that
// avoids a thundering herd of workers retrying in lockstep. math/rand/v2,
// not the forbidigo-banned math/rand: this backs no security decision
// (.golangci.yml's own ban rationale), exactly like
// tests/property/paired_result_test.go's generator.
type BackoffConfig struct {
	Base time.Duration
	Max  time.Duration
}

func (c BackoffConfig) withDefaults() BackoffConfig {
	if c.Base <= 0 {
		c.Base = 500 * time.Millisecond
	}
	if c.Max <= 0 {
		c.Max = 5 * time.Minute
	}
	return c
}

// Delay returns the backoff for the given 1-indexed attempt number and logs
// (via the returned Reason) why — task 6.7's "backoff + jitter logged with
// a reason." rng defaults to a package-level source if nil.
type Delay struct {
	Duration time.Duration
	Reason   string
}

func (c BackoffConfig) Delay(attempt int, rng *rand.Rand) Delay {
	c = c.withDefaults()
	if attempt < 1 {
		attempt = 1
	}
	ceiling := c.Base * time.Duration(1<<min(attempt, 20)) // bounded by min(attempt,20); never overflows a time.Duration shift
	if ceiling > c.Max || ceiling <= 0 {
		ceiling = c.Max
	}
	if rng == nil {
		rng = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0)) //nolint:gosec // jitter, not a security decision
	}
	d := time.Duration(rng.Int64N(int64(ceiling) + 1))
	return Delay{Duration: d, Reason: fmt.Sprintf("attempt %d: exponential backoff capped at %s, jittered to %s", attempt, ceiling, d)}
}

// defaultBreakerCooldown is how long an open breaker stays open before it lets
// one more attempt through.
const defaultBreakerCooldown = 5 * time.Minute

// CircuitBreaker trips open after `threshold` CONSECUTIVE failures that all
// carry the same reason string (task 6.7: "circuit break after 3 identical
// failures") — a run of failures with DIFFERENT reasons never trips it,
// since that looks like varied transient trouble rather than one
// deterministically-reproducing problem. Safe for concurrent use; one
// instance is meant to be shared per failure-domain (internal/queue keys
// one per session_key, not one per process — a single stuck session must
// never trip the breaker for every OTHER session sharing the worker pool).
//
// An open breaker closes by itself after its cool-down: the next attempt is let
// through, and one more identical failure opens it again for another cool-down.
// Without that, a breaker opened on a failure that has since been fixed (a provider
// outage, a bad deploy) refused its session for as long as the process lived, since
// only a success could close it and an open breaker never runs the job.
type CircuitBreaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time // test seam; nil means time.Now

	mu          sync.Mutex
	consecutive int
	lastReason  string
	open        bool
	openedAt    time.Time
}

func NewCircuitBreaker(threshold int) *CircuitBreaker {
	if threshold <= 0 {
		threshold = 3
	}
	return &CircuitBreaker{threshold: threshold, cooldown: defaultBreakerCooldown}
}

func (b *CircuitBreaker) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// expireLocked closes the breaker once its cool-down has passed, leaving it one
// identical failure short of tripping again.
func (b *CircuitBreaker) expireLocked() {
	if b.open && b.cooldown > 0 && b.clock().Sub(b.openedAt) >= b.cooldown {
		b.open = false
		b.consecutive = b.threshold - 1
	}
}

// RecordFailure folds in one more failure and returns true the moment the
// breaker trips open (i.e., only on the transition, not on every call while
// already open) so a caller logs the trip exactly once.
func (b *CircuitBreaker) RecordFailure(reason string) (tripped bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	if b.open {
		return false
	}
	if reason == b.lastReason {
		b.consecutive++
	} else {
		b.lastReason = reason
		b.consecutive = 1
	}
	if b.consecutive >= b.threshold {
		b.open = true
		b.openedAt = b.clock()
		return true
	}
	return false
}

// RecordSuccess resets the breaker entirely — a single success is proof the
// deterministic-failure hypothesis RecordFailure was accumulating evidence
// for no longer holds.
func (b *CircuitBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive = 0
	b.lastReason = ""
	b.open = false
}

// Open reports whether the breaker is refusing attempts: tripped, and not yet past
// its cool-down.
func (b *CircuitBreaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	return b.open
}

// BreakerRegistry hands out one CircuitBreaker per key (a session_key, in
// internal/queue's usage), created lazily on first touch — the same
// per-key-lazy-map idiom internal/tools.Pipeline's own sessions map and
// internal/cost.Gate's own caches already use.
//
// One registry is shared by every worker in a process, so a session's failures are
// counted together whichever worker ran them, and Forget drops a key once its
// session has succeeded: the map would otherwise grow by one entry per session for
// the life of the process.
type BreakerRegistry struct {
	threshold int
	// Cooldown is how long a tripped breaker stays open; 0 means five minutes. Set
	// it before the registry is first used.
	Cooldown time.Duration
	now      func() time.Time // test seam, handed to every breaker; nil means time.Now

	mu       sync.Mutex
	breakers map[string]*CircuitBreaker
}

func NewBreakerRegistry(threshold int) *BreakerRegistry {
	return &BreakerRegistry{threshold: threshold, breakers: map[string]*CircuitBreaker{}}
}

func (r *BreakerRegistry) For(key string) *CircuitBreaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.breakers[key]
	if !ok {
		b = NewCircuitBreaker(r.threshold)
		if r.Cooldown > 0 {
			b.cooldown = r.Cooldown
		}
		b.now = r.now
		r.breakers[key] = b
	}
	return b
}

// Forget drops key's breaker. A key that comes back starts with a fresh one.
func (r *BreakerRegistry) Forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.breakers, key)
}

// Len is how many keys the registry is tracking.
func (r *BreakerRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.breakers)
}
