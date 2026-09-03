package telegram

import (
	"sync"
	"time"
)

// RateLimiter is a small in-memory token bucket per external identity
// (docs/constitution.md: "rate-limit per external identity BEFORE the
// kernel sees the payload") — one bucket per key, refilled continuously at
// rate tokens/refillEvery, capped at burst. In-memory and per-process is
// the honest interim this codebase's other fixed-cap seams already accept
// (e.g. internal/reliability's circuit breaker) rather than a distributed
// limiter this phase doesn't need yet.
type RateLimiter struct {
	burst       int
	refillEvery time.Duration

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens   int
	lastFill time.Time
}

func NewRateLimiter(burst int, refillEvery time.Duration) *RateLimiter {
	if burst <= 0 {
		burst = 20
	}
	if refillEvery <= 0 {
		refillEvery = time.Minute
	}
	return &RateLimiter{burst: burst, refillEvery: refillEvery, buckets: map[string]*bucket{}}
}

// Allow reports whether key may proceed right now, consuming one token if
// so. A key seen for the first time starts with a full bucket — the first
// burst of traffic from a brand-new identity is not itself suspicious.
func (r *RateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	b, ok := r.buckets[key]
	now := time.Now()
	if !ok {
		b = &bucket{tokens: r.burst - 1, lastFill: now}
		r.buckets[key] = b
		return true
	}

	elapsed := now.Sub(b.lastFill)
	if refills := int(elapsed / r.refillEvery); refills > 0 {
		b.tokens = min(b.tokens+refills, r.burst)
		b.lastFill = now
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}
