package email

import (
	"testing"
	"time"
)

func TestRateLimiter_BurstThenRefuse(t *testing.T) {
	r := NewRateLimiter(2, time.Hour)
	if !r.Allow("a") {
		t.Fatal("first call = false, want true (fresh bucket starts full)")
	}
	if !r.Allow("a") {
		t.Fatal("second call = false, want true (burst of 2)")
	}
	if r.Allow("a") {
		t.Fatal("third call = true, want false (burst exhausted, no refill yet)")
	}
}

func TestRateLimiter_DistinctKeysDoNotShareABucket(t *testing.T) {
	r := NewRateLimiter(1, time.Hour)
	if !r.Allow("a") {
		t.Fatal("Allow(a) = false, want true")
	}
	if !r.Allow("b") {
		t.Fatal("Allow(b) = false, want true — a different key must not be throttled by a's own consumption")
	}
}

func TestRateLimiter_RefillsOverTime(t *testing.T) {
	r := NewRateLimiter(1, time.Millisecond)
	if !r.Allow("a") {
		t.Fatal("first call = false, want true")
	}
	if r.Allow("a") {
		t.Fatal("second call immediately after = true, want false (not refilled yet)")
	}
	time.Sleep(5 * time.Millisecond)
	if !r.Allow("a") {
		t.Fatal("call after refillEvery has elapsed = false, want true")
	}
}
