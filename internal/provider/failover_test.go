package provider_test

import (
	"context"
	"strings"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
)

func drain(t *testing.T, s provider.Stream) ([]provider.Chunk, error) {
	t.Helper()
	var chunks []provider.Chunk
	for {
		c, ok, err := s.Next(context.Background())
		if err != nil {
			return chunks, err
		}
		if !ok {
			return chunks, nil
		}
		chunks = append(chunks, c)
	}
}

func TestFailoverRetriesBeforeFirstChunk(t *testing.T) {
	// Provider 1 refuses the call outright (ThrottleError, from Stream()
	// itself); nothing has reached the caller yet, so provider 2 must be
	// tried and its content returned.
	p1 := fake.New(fake.Script{ThrottleErr: true})
	p2 := fake.New(fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "content", Text: "from provider 2"},
		{Kind: "done", Done: "stop"},
	}})

	wrapped := provider.Wrap([]provider.Provider{p1, p2})
	stream, err := wrapped.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: unexpected error: %v", err)
	}
	chunks, err := drain(t, stream)
	if err != nil {
		t.Fatalf("drain: unexpected error: %v", err)
	}
	if len(chunks) != 2 || chunks[0].Text != "from provider 2" {
		t.Fatalf("expected provider 2's content, got %+v", chunks)
	}
}

func TestFailoverRetriesOnFirstNextError(t *testing.T) {
	// Provider 1's Stream() call succeeds, but its very first Next() call
	// fails with a retryable error (Truncate with zero chunks scripted:
	// fake reports io.ErrUnexpectedEOF immediately) — still before anything
	// reached the caller, so this must also fail over even though the
	// failure came from Next(), not from Stream() itself.
	p1 := fake.New(fake.Script{Truncate: true})
	p2 := fake.New(fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "content", Text: "from provider 2"},
		{Kind: "done", Done: "stop"},
	}})

	wrapped := provider.Wrap([]provider.Provider{p1, p2})
	stream, err := wrapped.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: unexpected error: %v", err)
	}
	chunks, err := drain(t, stream)
	if err != nil {
		t.Fatalf("drain: unexpected error: %v", err)
	}
	if len(chunks) != 2 || chunks[0].Text != "from provider 2" {
		t.Fatalf("expected provider 2's content, got %+v", chunks)
	}
}

func TestFailoverDoesNotRetryPermanentFirstNextError(t *testing.T) {
	// Provider 1's very first Next() call fails, but with an untyped error
	// (Malformed, with exactly one chunk scripted so it fails immediately) —
	// classified Permanent by default (ClassifyTrigger only recognizes
	// specific typed/sentinel errors as retryable; an unrecognized failure
	// shape must not be assumed safe to retry). Provider 2 must never be
	// tried even though nothing had reached the caller yet.
	p1 := fake.New(fake.Script{Chunks: []fake.ChunkSpec{{Kind: "content", Text: "never returned"}}, Malformed: true})
	p2 := fake.New() // zero scripts — must never be called

	wrapped := provider.Wrap([]provider.Provider{p1, p2})
	_, err := wrapped.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err == nil {
		t.Fatal("expected the malformed-stream error to surface, got nil")
	}
	if strings.Contains(err.Error(), "no script left") {
		t.Fatalf("failover incorrectly retried a permanent-classified error and called provider 2: %v", err)
	}
	if provider.ClassifyTrigger(err) != provider.TriggerPermanent {
		t.Fatalf("expected TriggerPermanent, got %v", provider.ClassifyTrigger(err))
	}
}

func TestFailoverDoesNotRetryAfterFirstChunk(t *testing.T) {
	// Provider 1 delivers one real chunk (reaches the caller — committed),
	// then breaks. Provider 2 is configured with NO scripts at all: if
	// Wrap incorrectly failed over post-commit, draining would surface
	// fake's "no script left" error instead of the malformed one below,
	// which is how this test tells the two apart.
	p1 := fake.New(fake.Script{
		Chunks:    []fake.ChunkSpec{{Kind: "content", Text: "first chunk"}, {Kind: "content", Text: "unused"}},
		Malformed: true,
	})
	p2 := fake.New() // zero scripts — must never be called

	wrapped := provider.Wrap([]provider.Provider{p1, p2})
	stream, err := wrapped.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: unexpected error: %v", err)
	}
	chunks, err := drain(t, stream)
	if err == nil {
		t.Fatal("expected the malformed-stream error to surface, got nil")
	}
	if strings.Contains(err.Error(), "no script left") {
		t.Fatalf("failover ran past the commit point and called provider 2: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Text != "first chunk" {
		t.Fatalf("expected exactly the one committed chunk before the error, got %+v", chunks)
	}
}

func TestFailoverNeverRetriesContextOverflow(t *testing.T) {
	p1 := contextOverflowProvider{}
	p2 := fake.New() // zero scripts — must never be called

	wrapped := provider.Wrap([]provider.Provider{p1, p2})
	_, err := wrapped.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err == nil {
		t.Fatal("expected a context overflow error, got nil")
	}
	if provider.ClassifyTrigger(err) != provider.TriggerContextOverflow {
		t.Fatalf("expected TriggerContextOverflow, got %v", provider.ClassifyTrigger(err))
	}
}

// contextOverflowProvider is a minimal Provider whose Stream always refuses
// with ContextOverflowError — provider/fake has no scripted way to produce
// this trigger class, and giving it one isn't warranted just for this test.
type contextOverflowProvider struct{}

func (contextOverflowProvider) Stream(context.Context, provider.Prompt, []provider.ToolSchema, provider.RunContext) (provider.Stream, error) {
	return nil, &provider.ContextOverflowError{Reason: "prompt exceeds context window"}
}

func TestClassifyTrigger(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want provider.TriggerClass
	}{
		{"throttle", &provider.ThrottleError{Reason: "x"}, provider.TriggerRetryable},
		{"context overflow", &provider.ContextOverflowError{Reason: "x"}, provider.TriggerContextOverflow},
		{"unknown", errUnknown, provider.TriggerPermanent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := provider.ClassifyTrigger(c.err); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

var errUnknown = &customErr{"unknown failure"}

type customErr struct{ msg string }

func (e *customErr) Error() string { return e.msg }
