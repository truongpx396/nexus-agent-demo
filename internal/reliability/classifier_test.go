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
		var max time.Duration
		for i := 0; i < 200; i++ {
			if d := cfg.Delay(attempt, rng).Duration; d > max {
				max = d
			}
		}
		return max
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
	for i := 0; i < 10; i++ {
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
