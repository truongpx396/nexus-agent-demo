// Package fake is the deterministic recorded/fake Provider mandated by
// docs/constitution.md, Principle IX: correctness tests run against this,
// never a live model, so they neither flake nor bill. It also scripts the
// failure modes a real provider produces — truncation, stall, a malformed
// stream, and an outright throttle refusal — because the failover logic
// those conditions drive (Phase 2) needs a harness that can reproduce them
// on demand.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// ChunkSpec is the YAML/Go-literal shape of one scripted chunk. Input is a
// raw JSON string (not nested YAML) so a script file's tool_use payload is
// unambiguous.
type ChunkSpec struct {
	Kind      string `yaml:"kind" json:"kind"`
	Text      string `yaml:"text,omitempty" json:"text,omitempty"`
	ToolUseID string `yaml:"tool_use_id,omitempty" json:"tool_use_id,omitempty"`
	ToolName  string `yaml:"tool_name,omitempty" json:"tool_name,omitempty"`
	Input     string `yaml:"input,omitempty" json:"input,omitempty"`
	Done      string `yaml:"done,omitempty" json:"done,omitempty"`

	// Usage fields, meaningful only when Kind == "usage": a scripted turn's
	// token counts, split by class exactly the way a real provider's usage
	// chunk is (provider.Usage's own doc comment) — internal/cost, Phase 4,
	// is what actually consumes these; earlier phases left them unset and
	// got a zero-value Usage, which stays true for any script that still
	// omits them.
	InputUncached   int `yaml:"input_uncached,omitempty" json:"input_uncached,omitempty"`
	InputCacheRead  int `yaml:"input_cache_read,omitempty" json:"input_cache_read,omitempty"`
	InputCacheWrite int `yaml:"input_cache_write,omitempty" json:"input_cache_write,omitempty"`
	OutputTokens    int `yaml:"output_tokens,omitempty" json:"output_tokens,omitempty"`
}

// Script describes one deterministic simulated provider turn: the chunks it
// emits, plus knobs to simulate the failure modes a real stream produces.
type Script struct {
	Chunks []ChunkSpec `yaml:"chunks" json:"chunks"`

	// StallAfter, if > 0, hangs Next after emitting this many chunks until
	// ctx is cancelled — simulating an idle-stream abort.
	StallAfter int `yaml:"stall_after,omitempty" json:"stall_after,omitempty"`
	// Truncate ends the stream after Chunks without a terminal `done` chunk.
	Truncate bool `yaml:"truncate,omitempty" json:"truncate,omitempty"`
	// Malformed returns an error in place of the LAST scripted chunk,
	// simulating a stream that breaks mid-delivery.
	Malformed bool `yaml:"malformed,omitempty" json:"malformed,omitempty"`
	// ThrottleErr makes Stream() itself refuse the call outright, before any
	// chunk is produced — the failover taxonomy's retryable case.
	ThrottleErr bool `yaml:"throttle,omitempty" json:"throttle,omitempty"`
}

func toChunk(spec ChunkSpec) (provider.Chunk, error) {
	switch provider.ChunkKind(spec.Kind) {
	case provider.ChunkContent:
		return provider.Chunk{Kind: provider.ChunkContent, Text: spec.Text}, nil
	case provider.ChunkReasoning:
		return provider.Chunk{Kind: provider.ChunkReasoning, Opaque: []byte(spec.Text)}, nil
	case provider.ChunkToolUse:
		var input json.RawMessage
		if spec.Input != "" {
			input = json.RawMessage(spec.Input)
		}
		return provider.Chunk{
			Kind:      provider.ChunkToolUse,
			ToolUseID: spec.ToolUseID,
			ToolName:  spec.ToolName,
			Input:     input,
		}, nil
	case provider.ChunkUsage:
		return provider.Chunk{Kind: provider.ChunkUsage, Usage: provider.Usage{
			InputUncached:   spec.InputUncached,
			InputCacheRead:  spec.InputCacheRead,
			InputCacheWrite: spec.InputCacheWrite,
			OutputTokens:    spec.OutputTokens,
		}}, nil
	case provider.ChunkDone:
		return provider.Chunk{Kind: provider.ChunkDone, Done: provider.DoneReason(spec.Done)}, nil
	default:
		return provider.Chunk{}, fmt.Errorf("fake provider: unknown chunk kind %q", spec.Kind)
	}
}

// Provider replays Scripts in order, one per Stream() call — ordered and
// deterministic, never randomized, so a test's Nth call always gets the
// Nth script.
type Provider struct {
	mu      sync.Mutex
	scripts []Script
	idx     int
}

func New(scripts ...Script) *Provider {
	return &Provider{scripts: scripts}
}

func (p *Provider) Stream(_ context.Context, _ provider.Prompt, _ []provider.ToolSchema, _ provider.RunContext) (provider.Stream, error) {
	p.mu.Lock()
	if p.idx >= len(p.scripts) {
		p.mu.Unlock()
		return nil, fmt.Errorf("fake provider: no script left for call #%d", p.idx+1)
	}
	script := p.scripts[p.idx]
	p.idx++
	p.mu.Unlock()

	if script.ThrottleErr {
		return nil, &provider.ThrottleError{Reason: "scripted throttle"}
	}
	return &stream{script: script}, nil
}

type stream struct {
	script Script
	pos    int
}

func (s *stream) Next(ctx context.Context) (provider.Chunk, bool, error) {
	if s.script.StallAfter > 0 && s.pos == s.script.StallAfter {
		<-ctx.Done()
		return provider.Chunk{}, false, ctx.Err()
	}

	if s.pos >= len(s.script.Chunks) {
		if s.script.Truncate {
			return provider.Chunk{}, false, io.ErrUnexpectedEOF
		}
		return provider.Chunk{}, false, nil
	}

	if s.script.Malformed && s.pos == len(s.script.Chunks)-1 {
		s.pos++
		return provider.Chunk{}, false, errors.New("fake provider: malformed stream chunk")
	}

	spec := s.script.Chunks[s.pos]
	s.pos++
	chunk, err := toChunk(spec)
	if err != nil {
		return provider.Chunk{}, false, err
	}
	return chunk, true, nil
}
