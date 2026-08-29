package provider

import (
	"context"
	"errors"
	"io"
)

// TriggerClass is the typed taxonomy a Stream (or Provider.Stream) failure is
// classified into before any retry is attempted (README task 2.9;
// constitution Principle VIII — "every failure MUST be classified into a
// typed class before any retry").
type TriggerClass string

const (
	// TriggerRetryable covers throttling, timeouts, and a stream that drops
	// before producing anything — conditions another provider (or another
	// attempt at the same one) might not hit.
	TriggerRetryable TriggerClass = "retryable"
	// TriggerPermanent covers everything else: malformed responses, auth
	// failures, invalid requests — retrying deterministically reproduces the
	// same failure.
	TriggerPermanent TriggerClass = "permanent"
	// TriggerContextOverflow is never retried and never failed over, on any
	// provider, at any point — a smaller prompt fixes it, a different
	// backend does not.
	TriggerContextOverflow TriggerClass = "context_overflow"
)

// ClassifyTrigger classifies an error returned by Provider.Stream or a
// Stream's Next.
func ClassifyTrigger(err error) TriggerClass {
	var overflow *ContextOverflowError
	if errors.As(err, &overflow) {
		return TriggerContextOverflow
	}
	var throttle *ThrottleError
	if errors.As(err, &throttle) {
		return TriggerRetryable
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return TriggerRetryable
	}
	return TriggerPermanent
}

// Wrap returns a Provider that fails over across providers, in order, on a
// TriggerRetryable classification — but only until the first chunk of a
// stream has reached the caller (the "committed after first chunk" rule,
// README task 2.9): a caller may already have surfaced or acted on partial
// output, so failing over past that point would silently duplicate or lose
// content. The commit point is measured at the first successful Next() call,
// not at Stream() returning, since a provider can fail on its first chunk
// after having accepted the call. TriggerContextOverflow is never retried.
// Wrap gives up and returns the last error once every provider has been
// tried.
func Wrap(providers []Provider) Provider {
	return &failoverProvider{providers: providers}
}

type failoverProvider struct {
	providers []Provider
}

func (f *failoverProvider) Stream(ctx context.Context, p Prompt, tools []ToolSchema, rc RunContext) (Stream, error) {
	if len(f.providers) == 0 {
		return nil, errors.New("failover: no providers configured")
	}

	var lastErr error
	for i, prov := range f.providers {
		hasNext := i < len(f.providers)-1

		stream, err := prov.Stream(ctx, p, tools, rc)
		if err != nil {
			lastErr = err
			if hasNext && ClassifyTrigger(err) == TriggerRetryable {
				continue
			}
			return nil, err
		}

		// Peek the first chunk here, before returning to the caller: a
		// retryable failure on the FIRST Next() call has still not reached
		// the caller, so it is still eligible for failover.
		chunk, ok, nerr := stream.Next(ctx)
		if nerr != nil {
			lastErr = nerr
			if hasNext && ClassifyTrigger(nerr) == TriggerRetryable {
				continue
			}
			return nil, nerr
		}
		return &peekedStream{first: chunk, firstOK: ok, hasFirst: true, inner: stream}, nil
	}
	return nil, lastErr
}

// peekedStream replays the chunk Wrap already pulled to decide the stream is
// usable, then delegates every subsequent call directly to inner — no
// failover logic runs past this point, by construction: once hasFirst is
// consumed, this type holds no reference to the remaining providers at all.
type peekedStream struct {
	first    Chunk
	firstOK  bool
	hasFirst bool
	inner    Stream
}

func (s *peekedStream) Next(ctx context.Context) (Chunk, bool, error) {
	if s.hasFirst {
		s.hasFirst = false
		return s.first, s.firstOK, nil
	}
	return s.inner.Next(ctx)
}
