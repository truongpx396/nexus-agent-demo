package fake

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
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

func TestNormalContentToolUseDoneSequence(t *testing.T) {
	p := New(Script{
		Chunks: []ChunkSpec{
			{Kind: "content", Text: "let me check that"},
			{Kind: "tool_use", ToolUseID: "t1", ToolName: "file_read", Input: `{"path":"README.md"}`},
			{Kind: "done", Done: "stop"},
		},
	})

	s, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks, err := drain(t, s)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3: %+v", len(chunks), chunks)
	}
	if chunks[0].Kind != provider.ChunkContent || chunks[0].Text != "let me check that" {
		t.Errorf("chunk 0 = %+v", chunks[0])
	}
	if chunks[1].Kind != provider.ChunkToolUse || chunks[1].ToolName != "file_read" {
		t.Errorf("chunk 1 = %+v", chunks[1])
	}
	if chunks[2].Kind != provider.ChunkDone || chunks[2].Done != provider.DoneStop {
		t.Errorf("chunk 2 = %+v", chunks[2])
	}
}

func TestTruncationEndsWithoutADoneChunk(t *testing.T) {
	p := New(Script{
		Chunks:   []ChunkSpec{{Kind: "content", Text: "partial"}},
		Truncate: true,
	})
	s, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks, err := drain(t, s)
	if err == nil {
		t.Fatal("expected an error at the truncated end of stream, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks before truncation, want 1", len(chunks))
	}
	for _, c := range chunks {
		if c.Kind == provider.ChunkDone {
			t.Fatal("a truncated stream must never emit a done chunk")
		}
	}
}

func TestStallHangsUntilContextCancellation(t *testing.T) {
	p := New(Script{
		Chunks:     []ChunkSpec{{Kind: "content", Text: "..."}, {Kind: "done", Done: "stop"}},
		StallAfter: 1,
	})
	s, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	ctx := context.Background()
	c, ok, err := s.Next(ctx)
	if err != nil || !ok || c.Kind != provider.ChunkContent {
		t.Fatalf("first chunk = %+v, %v, %v", c, ok, err)
	}

	stallCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = s.Next(stallCtx)
	if err == nil {
		t.Fatal("expected the stalled Next to return an error on context cancellation")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("Next returned after %v — it did not actually stall", elapsed)
	}
}

func TestMalformedStreamErrorsOnTheLastChunk(t *testing.T) {
	p := New(Script{
		Chunks:    []ChunkSpec{{Kind: "content", Text: "ok"}, {Kind: "done", Done: "stop"}},
		Malformed: true,
	})
	s, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks, err := drain(t, s)
	if err == nil {
		t.Fatal("expected a malformed-stream error, got nil")
	}
	if len(chunks) != 1 {
		t.Fatalf("got %d good chunks before the malformed one, want 1", len(chunks))
	}
}

func TestThrottleErrorRefusesBeforeAnyChunk(t *testing.T) {
	p := New(Script{ThrottleErr: true})
	_, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err == nil {
		t.Fatal("expected a throttle error from Stream itself")
	}
	var throttleErr *provider.ThrottleError
	if !errors.As(err, &throttleErr) {
		t.Fatalf("err = %v (%T), want *provider.ThrottleError", err, err)
	}
}

func TestScriptsAreConsumedInOrder(t *testing.T) {
	p := New(
		Script{Chunks: []ChunkSpec{{Kind: "content", Text: "first"}, {Kind: "done", Done: "stop"}}},
		Script{Chunks: []ChunkSpec{{Kind: "content", Text: "second"}, {Kind: "done", Done: "stop"}}},
	)

	for _, want := range []string{"first", "second"} {
		s, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		chunks, err := drain(t, s)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if chunks[0].Text != want {
			t.Fatalf("got %q, want %q", chunks[0].Text, want)
		}
	}

	if _, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{}); err == nil {
		t.Fatal("expected an error once every scripted call is consumed")
	}
}
