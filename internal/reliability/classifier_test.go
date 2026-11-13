package reliability

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	if Classify(context.DeadlineExceeded) != FailureRetryable {
		t.Fatal("a deadline exceeded should classify retryable")
	}
	if Classify(&PermanentError{Err: errors.New("session gone")}) != FailurePermanent {
		t.Fatal("a PermanentError should classify permanent")
	}
	if Classify(errors.New("some unrecognized error")) != FailureRetryable {
		t.Fatal("an unrecognized error should default retryable, not permanent")
	}
}

func TestBackoffConfig_Delay_NeverExceedsMax(t *testing.T) {
	cfg := BackoffConfig{Base: 100 * time.Millisecond, Max: 2 * time.Second}
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // a unit-test generator, not a security decision — see tests/property/paired_result_test.go's own identical justification
	for attempt := 1; attempt <= 30; attempt++ {
		d := cfg.Delay(attempt, rng)
		if d.Duration > cfg.Max {
			t.Fatalf("attempt %d: delay %s exceeds Max %s", attempt, d.Duration, cfg.Max)
		}
		if d.Duration < 0 {
			t.Fatalf("attempt %d: negative delay %s", attempt, d.Duration)
		}
		if d.Reason == "" {
			t.Fatalf("attempt %d: Delay produced no reason (task 6.7 requires backoff logged with a reason)", attempt)
		}
	}
}

func TestBackoffConfig_Delay_GrowsWithAttempt(t *testing.T) {
	cfg := BackoffConfig{Base: 10 * time.Millisecond, Max: time.Hour}
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // a unit-test generator, not a security decision — see tests/property/paired_result_test.go's own identical justification
	// The jitter ceiling itself (not any one sampled draw) must grow —
	// sample many draws per attempt and compare maxima.
	maxAt := func(attempt int) time.Duration {
		var longest time.Duration
		for range 200 {
			longest = max(longest, cfg.Delay(attempt, rng).Duration)
		}
		return longest
	}
	if maxAt(1) >= maxAt(5) {
		t.Fatalf("backoff ceiling did not grow with attempt count: attempt1 max=%s attempt5 max=%s", maxAt(1), maxAt(5))
	}
}

func TestCircuitBreaker_TripsOnlyOnIdenticalConsecutiveFailures(t *testing.T) {
	b := NewCircuitBreaker(3)
	if b.RecordFailure("timeout") {
		t.Fatal("should not trip on failure 1")
	}
	if b.RecordFailure("timeout") {
		t.Fatal("should not trip on failure 2")
	}
	if b.Open() {
		t.Fatal("should not be open before threshold")
	}
	if !b.RecordFailure("timeout") {
		t.Fatal("should trip exactly on the 3rd identical failure")
	}
	if !b.Open() {
		t.Fatal("should report open after tripping")
	}
	if b.RecordFailure("timeout") {
		t.Fatal("should not report a second trip once already open")
	}
}

func TestCircuitBreaker_DifferentReasonsNeverTrip(t *testing.T) {
	b := NewCircuitBreaker(3)
	for i := range 10 {
		if b.RecordFailure("reason-varies") {
			t.Fatalf("varied-reason failures must never trip the breaker (i=%d)", i)
		}
		b.lastReason = "" // force the next call to look like a fresh reason
	}
	if b.Open() {
		t.Fatal("breaker should not be open after only varied-reason failures")
	}
}

func TestCircuitBreaker_SuccessResets(t *testing.T) {
	b := NewCircuitBreaker(3)
	b.RecordFailure("x")
	b.RecordFailure("x")
	b.RecordSuccess()
	if b.RecordFailure("x") {
		t.Fatal("a success must reset the consecutive count")
	}
	if b.RecordFailure("x") {
		t.Fatal("still should not trip: only 2 since the reset")
	}
	if !b.RecordFailure("x") {
		t.Fatal("should trip on the 3rd since the reset")
	}
}

func TestBreakerRegistry_IsolatesKeys(t *testing.T) {
	reg := NewBreakerRegistry(2)
	a := reg.For("session-a")
	a.RecordFailure("x")
	a.RecordFailure("x")
	if !a.Open() {
		t.Fatal("session-a's breaker should be open")
	}
	b := reg.For("session-b")
	if b.Open() {
		t.Fatal("session-b's breaker must be independent of session-a's")
	}
}

// fakeClock is a test seam for a breaker's cool-down.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func clockedBreaker(threshold int, cooldown time.Duration) (*CircuitBreaker, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	b := NewCircuitBreaker(threshold)
	b.cooldown, b.now = cooldown, clock.now
	return b, clock
}

// An open breaker never ran the job again, and only a success could close it, so a
// breaker opened on a failure that has since been fixed refused its session for the
// life of the process.
func TestCircuitBreaker_ClosesAfterItsCooldownAndOneMoreIdenticalFailureReopensIt(t *testing.T) {
	b, clock := clockedBreaker(3, 5*time.Minute)
	for range 3 {
		b.RecordFailure("provider overloaded")
	}
	if !b.Open() {
		t.Fatal("the breaker should be open after 3 identical failures")
	}

	clock.advance(4*time.Minute + 59*time.Second)
	if !b.Open() {
		t.Fatal("the breaker closed before its cool-down passed")
	}

	clock.advance(2 * time.Second)
	if b.Open() {
		t.Fatal("the breaker is still open after its cool-down")
	}

	// It is one identical failure short of tripping, not back at zero.
	if !b.RecordFailure("provider overloaded") {
		t.Fatal("one identical failure after the cool-down should open the breaker again")
	}
	if !b.Open() {
		t.Fatal("the breaker should be open again")
	}
	clock.advance(5 * time.Minute)
	if b.Open() {
		t.Fatal("the second open should have its own cool-down")
	}
}

func TestCircuitBreaker_ADifferentFailureAfterTheCooldownDoesNotReopenIt(t *testing.T) {
	b, clock := clockedBreaker(3, time.Minute)
	for range 3 {
		b.RecordFailure("provider overloaded")
	}
	clock.advance(time.Minute)
	if b.RecordFailure("a different problem") || b.Open() {
		t.Fatal("a different failure must count from one again, not trip the breaker")
	}
}

func TestCircuitBreaker_ASuccessAfterTheCooldownResetsIt(t *testing.T) {
	b, clock := clockedBreaker(3, time.Minute)
	for range 3 {
		b.RecordFailure("provider overloaded")
	}
	clock.advance(time.Minute)
	b.RecordSuccess()
	for range 2 {
		if b.RecordFailure("provider overloaded") {
			t.Fatal("after a success the count restarts: two failures must not trip a threshold of 3")
		}
	}
}

// RecordFailure must also see the cool-down has passed, not only Open.
func TestCircuitBreaker_RecordFailureAloneExpiresTheCooldown(t *testing.T) {
	b, clock := clockedBreaker(3, time.Minute)
	for range 3 {
		b.RecordFailure("x")
	}
	clock.advance(time.Minute)
	if !b.RecordFailure("x") {
		t.Fatal("RecordFailure past the cool-down should trip again without Open having been called")
	}
}

func TestBreakerRegistry_CooldownAndTheClockReachEveryBreakerAndForgetRemovesTheEntry(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	r := NewBreakerRegistry(2)
	r.Cooldown, r.now = 30*time.Second, clock.now

	b := r.For("s1")
	b.RecordFailure("x")
	b.RecordFailure("x")
	if !b.Open() {
		t.Fatal("the breaker should be open")
	}
	clock.advance(31 * time.Second)
	if b.Open() {
		t.Fatal("the registry's Cooldown did not reach its breaker")
	}

	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
	r.Forget("s1")
	if r.Len() != 0 {
		t.Fatalf("Len = %d after Forget, want 0", r.Len())
	}
	if r.For("s1") == b {
		t.Fatal("a forgotten key must start with a fresh breaker")
	}
	r.Forget("never-seen") // a key that was never there is fine
}

func TestNewBreakerRegistry_DefaultCooldownIsFiveMinutes(t *testing.T) {
	if got := NewBreakerRegistry(3).For("s").cooldown; got != 5*time.Minute {
		t.Fatalf("default cool-down = %v, want 5m", got)
	}
}
