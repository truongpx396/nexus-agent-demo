package provider

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// step is one scripted call to Provider.Stream: it fails outright, or returns a
// stream whose first read fails, or one that yields chunks and then maybe fails.
type step struct {
	streamErr error
	firstErr  error
	chunks    []Chunk
	afterErr  error // returned by the read after the chunks, if set
}

type scriptedStream struct {
	st  step
	pos int
}

func (s *scriptedStream) Next(context.Context) (Chunk, bool, error) {
	if s.pos == 0 && s.st.firstErr != nil {
		return Chunk{}, false, s.st.firstErr
	}
	if s.pos < len(s.st.chunks) {
		c := s.st.chunks[s.pos]
		s.pos++
		return c, true, nil
	}
	if s.st.afterErr != nil {
		return Chunk{}, false, s.st.afterErr
	}
	return Chunk{}, false, nil
}

type scriptedProvider struct {
	steps []step
	calls int
	model string
}

func (p *scriptedProvider) Stream(context.Context, Prompt, []ToolSchema, RunContext) (Stream, error) {
	st := p.steps[min(p.calls, len(p.steps)-1)]
	p.calls++
	if st.streamErr != nil {
		return nil, st.streamErr
	}
	return &scriptedStream{st: st}, nil
}

func (p *scriptedProvider) BoundModel() string { return p.model }

var okStep = step{chunks: []Chunk{{Kind: ChunkContent, Text: "hi"}, {Kind: ChunkDone, Done: DoneStop}}}

// recorder is a RetryConfig whose Sleep records the delays instead of waiting.
func recorder(attempts int) (RetryConfig, *[]time.Duration, *[]int) {
	var delays []time.Duration
	var retries []int
	return RetryConfig{
		Attempts: attempts, Base: 100 * time.Millisecond, Max: time.Second,
		Sleep:   func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil },
		OnRetry: func(attempt int, _ error, _ time.Duration) { retries = append(retries, attempt) },
	}, &delays, &retries
}

func drain(t *testing.T, s Stream) []Chunk {
	t.Helper()
	var out []Chunk
	for {
		c, ok, err := s.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			return out
		}
		out = append(out, c)
	}
}

func TestWithRetry_AThrottleThenSuccessReturnsTheStreamAfterOneRetry(t *testing.T) {
	inner := &scriptedProvider{steps: []step{{streamErr: &ThrottleError{Reason: "overloaded"}}, okStep}}
	cfg, delays, retries := recorder(3)

	s, err := WithRetry(inner, cfg).Stream(context.Background(), Prompt{}, nil, RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := drain(t, s); len(got) != 2 || got[0].Text != "hi" {
		t.Fatalf("chunks = %+v, want the successful call's, first chunk included", got)
	}
	if inner.calls != 2 || len(*delays) != 1 || len(*retries) != 1 || (*retries)[0] != 1 {
		t.Fatalf("calls=%d delays=%v retries=%v, want two calls, one wait, retry number 1", inner.calls, *delays, *retries)
	}
	if d := (*delays)[0]; d < 0 || d >= 100*time.Millisecond {
		t.Fatalf("first delay %v is outside full jitter [0, Base)", d)
	}
}

// A provider can accept the call and fail on its first read: still before the
// caller has anything, so still retryable.
func TestWithRetry_AFailureOnTheFirstReadIsRetried(t *testing.T) {
	inner := &scriptedProvider{steps: []step{{firstErr: io.ErrUnexpectedEOF}, okStep}}
	cfg, _, _ := recorder(3)

	s, err := WithRetry(inner, cfg).Stream(context.Background(), Prompt{}, nil, RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := drain(t, s); len(got) != 2 {
		t.Fatalf("chunks = %+v", got)
	}
	if inner.calls != 2 {
		t.Fatalf("calls = %d, want 2", inner.calls)
	}
}

func TestWithRetry_ThreeThrottlesReturnTheLastError(t *testing.T) {
	last := &ThrottleError{Reason: "third"}
	inner := &scriptedProvider{steps: []step{
		{streamErr: &ThrottleError{Reason: "first"}}, {streamErr: &ThrottleError{Reason: "second"}}, {streamErr: last},
	}}
	cfg, delays, _ := recorder(3)

	_, err := WithRetry(inner, cfg).Stream(context.Background(), Prompt{}, nil, RunContext{})
	if !errors.Is(err, error(last)) {
		t.Fatalf("err = %v, want the last throttle", err)
	}
	if inner.calls != 3 || len(*delays) != 2 {
		t.Fatalf("calls=%d waits=%d, want 3 calls and a wait between each, none after the last", inner.calls, len(*delays))
	}
	// The ceiling doubles per retry: [0,100ms) then [0,200ms).
	if (*delays)[0] >= 100*time.Millisecond || (*delays)[1] >= 200*time.Millisecond {
		t.Fatalf("delays %v exceed the exponential ceiling", *delays)
	}
}

func TestWithRetry_PermanentAndContextOverflowAreNeverRetried(t *testing.T) {
	for name, e := range map[string]error{
		"permanent":        errors.New("anthropic: invalid_request_error: bad tool"),
		"context overflow": &ContextOverflowError{Reason: "prompt is too long"},
	} {
		inner := &scriptedProvider{steps: []step{{streamErr: e}, okStep}}
		cfg, delays, _ := recorder(3)
		if _, err := WithRetry(inner, cfg).Stream(context.Background(), Prompt{}, nil, RunContext{}); !errors.Is(err, e) {
			t.Errorf("%s: err = %v, want %v", name, err, e)
		}
		if inner.calls != 1 || len(*delays) != 0 {
			t.Errorf("%s: calls=%d waits=%d, want exactly one call and no wait", name, inner.calls, len(*delays))
		}
	}
}

// Past the first chunk the caller may have acted on partial output, so a failure
// is the caller's to see.
func TestWithRetry_AnErrorAfterTheFirstChunkIsNotRetried(t *testing.T) {
	inner := &scriptedProvider{steps: []step{{chunks: []Chunk{{Kind: ChunkContent, Text: "par"}}, afterErr: io.ErrUnexpectedEOF}, okStep}}
	cfg, delays, _ := recorder(3)

	s, err := WithRetry(inner, cfg).Stream(context.Background(), Prompt{}, nil, RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if c, ok, err := s.Next(context.Background()); err != nil || !ok || c.Text != "par" {
		t.Fatalf("first chunk = %+v, %v, %v", c, ok, err)
	}
	if _, _, err := s.Next(context.Background()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("second read err = %v, want the stream's own error", err)
	}
	if inner.calls != 1 || len(*delays) != 0 {
		t.Fatalf("calls=%d waits=%d, want no retry once a chunk was delivered", inner.calls, len(*delays))
	}
}

func TestWithRetry_ARetryAfterRaisesTheDelayAndIsCappedAtMax(t *testing.T) {
	inner := &scriptedProvider{steps: []step{
		{streamErr: &ThrottleError{Reason: "slow down", RetryAfter: 5 * time.Second}},
		{streamErr: &ThrottleError{Reason: "much slower", RetryAfter: time.Hour}},
		okStep,
	}}
	cfg, delays, _ := recorder(3)
	cfg.Max = 8 * time.Second

	if _, err := WithRetry(inner, cfg).Stream(context.Background(), Prompt{}, nil, RunContext{}); err != nil {
		t.Fatal(err)
	}
	if len(*delays) != 2 || (*delays)[0] < 5*time.Second || (*delays)[1] != 8*time.Second {
		t.Fatalf("delays = %v, want at least the 5s the provider asked for, then capped at Max (8s)", *delays)
	}
}

func TestWithRetry_ACancelledContextStopsTheLoop(t *testing.T) {
	inner := &scriptedProvider{steps: []step{{streamErr: &ThrottleError{Reason: "overloaded"}}, okStep}}
	ctx, cancel := context.WithCancel(context.Background())
	cfg, _, _ := recorder(5)
	cfg.Sleep = func(ctx context.Context, _ time.Duration) error {
		cancel() // the run is cancelled while waiting to retry
		return ctx.Err()
	}

	_, err := WithRetry(inner, cfg).Stream(ctx, Prompt{}, nil, RunContext{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if inner.calls != 1 {
		t.Fatalf("calls = %d, want 1: a cancelled run must not call the provider again", inner.calls)
	}

	// An already-cancelled context is not retried at all.
	inner2 := &scriptedProvider{steps: []step{{streamErr: &ThrottleError{Reason: "overloaded"}}, okStep}}
	cfg2, delays, _ := recorder(5)
	if _, err := WithRetry(inner2, cfg2).Stream(ctx, Prompt{}, nil, RunContext{}); err == nil {
		t.Fatal("expected an error")
	}
	if inner2.calls != 1 || len(*delays) != 0 {
		t.Fatalf("calls=%d waits=%d, want one call and no wait", inner2.calls, len(*delays))
	}
}

func TestWithRetry_PassesTheBoundModelThrough(t *testing.T) {
	bound := WithRetry(&scriptedProvider{model: "claude-sonnet-5", steps: []step{okStep}}, RetryConfig{})
	if got := ServedModel(bound, "routed"); got != "claude-sonnet-5" {
		t.Fatalf("ServedModel through the wrapper = %q, want the bound model", got)
	}
	if b, ok := bound.(ModelBound); !ok || b.BoundModel() != "claude-sonnet-5" {
		t.Fatal("the wrapper does not report the model it is bound to")
	}
	// A chain of one wrapped provider still prices under the bound model.
	if got := ServedModel(Wrap([]Provider{bound}), "routed"); got != "claude-sonnet-5" {
		t.Fatalf("ServedModel through Wrap(WithRetry(p)) = %q, want the bound model", got)
	}
	// A provider with no model of its own leaves the routed one standing.
	if got := ServedModel(WithRetry(&scriptedProvider{steps: []step{okStep}}, RetryConfig{}), "routed"); got != "routed" {
		t.Fatalf("ServedModel with an unbound inner = %q, want the routed id", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"5": 5 * time.Second, " 12 ": 12 * time.Second, "": 0, "0": 0, "-3": 0,
		"Wed, 21 Oct 2026 07:28:00 GMT": 0, "1.5": 0,
	} {
		if got := ParseRetryAfter(in); got != want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestWithRetry_DefaultsApply(t *testing.T) {
	r, ok := WithRetry(&scriptedProvider{steps: []step{okStep}}, RetryConfig{}).(*retryProvider)
	if !ok {
		t.Fatal("WithRetry did not return a *retryProvider")
	}
	if r.cfg.Attempts != 3 || r.cfg.Base != time.Second || r.cfg.Max != 20*time.Second || r.cfg.Sleep == nil {
		t.Fatalf("defaults = %+v", r.cfg)
	}
	// The real sleep honours the context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.cfg.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep on a cancelled context = %v", err)
	}
}
