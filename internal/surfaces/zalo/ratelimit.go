package zalo

import (
	"sync"
	"time"
)

// RateLimiter is internal/surfaces/telegram.RateLimiter's own duplicate
// (its doc comment) — per this codebase's established cross-surface idiom,
// not a shared package, since the two surfaces have no other reason to
// depend on each other.
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
