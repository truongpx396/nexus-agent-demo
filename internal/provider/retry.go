package provider

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// Defaults for WithRetry. A model call that is throttled or overloaded usually
// clears within seconds, so a few jittered retries absorb it; waiting longer
// than Max holds a worker and its session lock for little gain.
const (
	defaultRetryAttempts = 3
	defaultRetryBase     = time.Second
	defaultRetryMax      = 20 * time.Second
)

// RetryConfig configures WithRetry. The zero value is valid and uses the
// defaults above.
type RetryConfig struct {
	// Attempts is the total number of tries, the first included. <= 0 means 3.
	Attempts int
	// Base and Max bound the backoff: before retry n (0-based) the delay is a
	// random duration in [0, min(Max, Base<<n)), "full jitter", so workers that
	// were throttled together do not retry together. <= 0 means 1s and 20s.
	Base, Max time.Duration
	// Sleep waits for d or returns the context's error if it ends first. Nil
	// means a timer; tests pass a recorder so they do not wait.
	Sleep func(ctx context.Context, d time.Duration) error
	// OnRetry is called before each wait with the 1-based number of the retry
	// about to happen, the error that caused it and the delay. Nil is valid.
	OnRetry func(attempt int, err error, delay time.Duration)
}

// WithRetry returns a Provider that retries p when its call fails with a
// TriggerRetryable error before the first chunk has reached the caller: a
// throttle, an overload, a timeout, a dropped connection. It is the
// "classify, then retry with a logged reason and jittered backoff" that
// constitution Principle VIII requires, for the case failover cannot help with,
// a chain with one provider in it.
//
// Like Wrap, it stops being able to help at the first chunk: a caller may already
// have surfaced or acted on partial output, so a stream that fails afterwards is
// returned to the caller as it is. A context-overflow or permanent error is never
// retried, and neither is anything once the context has ended.
func WithRetry(p Provider, cfg RetryConfig) Provider {
	if cfg.Attempts <= 0 {
		cfg.Attempts = defaultRetryAttempts
	}
	if cfg.Base <= 0 {
		cfg.Base = defaultRetryBase
	}
	if cfg.Max <= 0 {
		cfg.Max = defaultRetryMax
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	return &retryProvider{inner: p, cfg: cfg}
}

type retryProvider struct {
	inner Provider
	cfg   RetryConfig
}

// BoundModel implements ModelBound by delegating to the wrapped provider. Without
// it the wrapper would hide the model the provider is bound to, and a run would be
// priced under the one the router chose rather than the one that served it.
func (r *retryProvider) BoundModel() string {
	if b, ok := r.inner.(ModelBound); ok {
		return b.BoundModel()
	}
	return ""
}

func (r *retryProvider) Stream(ctx context.Context, p Prompt, tools []ToolSchema, rc RunContext) (Stream, error) {
	for attempt := range r.cfg.Attempts {
		stream, err := r.inner.Stream(ctx, p, tools, rc)
		if err == nil {
			// The first chunk is pulled here, as Wrap does: a provider can accept
			// the call and then fail on its first read, which has still not
			// reached the caller.
			var first Chunk
			var ok bool
			if first, ok, err = stream.Next(ctx); err == nil {
				return &peekedStream{first: first, firstOK: ok, hasFirst: true, inner: stream}, nil
			}
		}

		if attempt == r.cfg.Attempts-1 || ctx.Err() != nil || ClassifyTrigger(err) != TriggerRetryable {
			return nil, err
		}
		delay := r.backoff(attempt, err)
		if r.cfg.OnRetry != nil {
			r.cfg.OnRetry(attempt+1, err, delay)
		}
		if serr := r.cfg.Sleep(ctx, delay); serr != nil {
			return nil, serr
		}
	}
	return nil, errors.New("provider: retry: no attempts configured") // unreachable: Attempts >= 1
}

// backoff is the wait before retry number retry (0-based): full jitter below an
// exponential ceiling, raised to the provider's own Retry-After when it sent one,
// but never past Max.
func (r *retryProvider) backoff(retry int, err error) time.Duration {
	ceiling := r.cfg.Max
	if retry < 32 {
		if exp := r.cfg.Base << retry; exp > 0 && exp < ceiling {
			ceiling = exp
		}
	}
	var d time.Duration
	if ceiling > 0 {
		d = rand.N(ceiling) //nolint:gosec // retry jitter, not a security decision
	}
	if te, ok := errors.AsType[*ThrottleError](err); ok && te.RetryAfter > d {
		d = min(te.RetryAfter, r.cfg.Max)
	}
	return d
}

// sleepCtx waits for d or until ctx ends, whichever is first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ParseRetryAfter reads a Retry-After header's value in its delay-seconds form
// ("5"), which is what the hosted APIs send. The HTTP-date form, a negative
// number or anything else is zero, meaning the provider gave no usable hint. It
// takes the header's string rather than an http.Header because this package does
// no network I/O; the adapter that has the response passes the value in.
func ParseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
