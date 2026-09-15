package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// Stream issues the request and returns a normalized Stream. An error
// returned here (never via the Stream it would have returned) means the API
// refused the call outright — mirrors provider.ThrottleError's documented
// contract.
func (p *Provider) Stream(ctx context.Context, prompt provider.Prompt, tools []provider.ToolSchema, _ provider.RunContext) (provider.Stream, error) {
	reqBody := messagesRequest{
		Model:     p.Model,
		MaxTokens: p.maxTokens(),
		System:    toAnthSystem(prompt.System),
		Messages:  toAnthMessages(prompt.Messages),
		Tools:     toAnthTools(tools),
		Stream:    true,
		Thinking:  thinkingConfigFor(p.Model),
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL()+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", p.APIKey)
	httpReq.Header.Set("anthropic-version", apiVersion)

	resp, err := p.client().Do(httpReq)
	if err != nil {
		return nil, &provider.ThrottleError{Reason: fmt.Sprintf("request failed: %v", err)}
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close() //nolint:errcheck // best-effort close on an already-failed request
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		var eb errorBody
		_ = json.Unmarshal(body, &eb) // best-effort; classifyAPIError falls back to the status code alone
		return nil, classifyAPIError(resp.StatusCode, eb)
	}

	// README task 13.14 (closing production-readiness finding F14): the
	// default bufio.Scanner line cap is 64KB, which hard-errors (rather than
	// degrading) on an oversized SSE line — a single content_block_delta or
	// input_json_delta frame carrying a large chunk of text/partial JSON can
	// exceed that. 1MB covers any realistic single-line SSE frame this API
	// sends.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	return &stream{
		body: resp.Body, scanner: scanner,
		toolBlocks: map[int]*toolBlockState{}, thinkingBlocks: map[int]*thinkingBlockState{},
	}, nil
}

type toolBlockState struct {
	id, name string
	json     bytes.Buffer
}

// thinkingBlockState accumulates one extended-thinking content block's
// thinking_delta/signature_delta events (README task 13.7) — mirrors
// toolBlockState's own per-index accumulate-until-content_block_stop shape.
type thinkingBlockState struct {
	thinking, signature strings.Builder
}

// thinkingOpaque is what a ChunkReasoning chunk's Opaque bytes decode as —
// kernel/rehydrate.go's reasoningOpaque mirrors this field-for-field (same
// JSON shape, independently defined per package, this codebase's usual
// convention for a payload two packages agree on without sharing a type).
type thinkingOpaque struct {
	Thinking  string `json:"thinking"`
	Signature string `json:"signature,omitempty"`
}

type stream struct {
	body           io.ReadCloser
	scanner        *bufio.Scanner
	toolBlocks     map[int]*toolBlockState
	thinkingBlocks map[int]*thinkingBlockState
	usage          provider.Usage
	pending        []provider.Chunk // FIFO of chunks decoded from one SSE frame, drained before reading the next frame
	closed         bool
}

// Next decodes one SSE frame at a time from the response body, translating
// it into zero or more normalized Chunks (an input_json_delta accumulates
// silently until its content_block_stop, which is where the one ChunkToolUse
// for that block is emitted).
func (s *stream) Next(ctx context.Context) (provider.Chunk, bool, error) {
	for {
		if len(s.pending) > 0 {
			c := s.pending[0]
			s.pending = s.pending[1:]
			return c, true, nil
		}
		if s.closed {
			return provider.Chunk{}, false, nil
		}
		if err := ctx.Err(); err != nil {
			return provider.Chunk{}, false, err
		}

		eventType, data, ok, err := readSSEFrame(s.scanner)
		if err != nil {
			return provider.Chunk{}, false, fmt.Errorf("anthropic: read stream: %w", err)
		}
		if !ok {
			// Clean EOF without a message_stop we recognized as done — the
			// contract callers rely on (provider.Stream's doc comment) is
			// that ChunkDone is emitted before a clean end on any non-error
			// path; a stream that ends here without one is truncated.
			_ = s.body.Close()
			return provider.Chunk{}, false, io.ErrUnexpectedEOF
		}

		var evt sseEvent
		if err := json.Unmarshal(data, &evt); err != nil {
			return provider.Chunk{}, false, fmt.Errorf("anthropic: parse SSE event %q: %w", eventType, err)
		}

		switch eventType {
		case "message_start":
			s.usage.InputUncached = evt.Message.Usage.InputTokens
			s.usage.InputCacheWrite = evt.Message.Usage.CacheCreationInputTokens
			s.usage.InputCacheRead = evt.Message.Usage.CacheReadInputTokens
		case "content_block_start":
			switch evt.ContentBlock.Type {
			case "tool_use":
				s.toolBlocks[evt.Index] = &toolBlockState{id: evt.ContentBlock.ID, name: evt.ContentBlock.Name}
			case "thinking":
				s.thinkingBlocks[evt.Index] = &thinkingBlockState{}
			}
		case "content_block_delta":
			switch evt.Delta.Type {
			case "text_delta":
				s.pending = append(s.pending, provider.Chunk{Kind: provider.ChunkContent, Text: evt.Delta.Text})
			case "input_json_delta":
				if tb, ok := s.toolBlocks[evt.Index]; ok {
					tb.json.WriteString(evt.Delta.PartialJSON)
				}
			case "thinking_delta":
				if tb, ok := s.thinkingBlocks[evt.Index]; ok {
					tb.thinking.WriteString(evt.Delta.Thinking)
				}
			case "signature_delta":
				if tb, ok := s.thinkingBlocks[evt.Index]; ok {
					tb.signature.WriteString(evt.Delta.Signature)
				}
			}
		case "content_block_stop":
			if tb, ok := s.toolBlocks[evt.Index]; ok {
				input := json.RawMessage(tb.json.Bytes())
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				s.pending = append(s.pending, provider.Chunk{
					Kind: provider.ChunkToolUse, ToolUseID: tb.id, ToolName: tb.name, Input: input,
				})
				delete(s.toolBlocks, evt.Index)
			}
			if tb, ok := s.thinkingBlocks[evt.Index]; ok {
				opaque, err := json.Marshal(thinkingOpaque{Thinking: tb.thinking.String(), Signature: tb.signature.String()})
				if err != nil {
					return provider.Chunk{}, false, fmt.Errorf("anthropic: marshal thinking block: %w", err)
				}
				s.pending = append(s.pending, provider.Chunk{Kind: provider.ChunkReasoning, Opaque: opaque})
				delete(s.thinkingBlocks, evt.Index)
			}
		case "message_delta":
			s.usage.OutputTokens = evt.Usage.OutputTokens
			s.pending = append(s.pending, provider.Chunk{Kind: provider.ChunkUsage, Usage: s.usage})
			s.pending = append(s.pending, provider.Chunk{
				Kind: provider.ChunkDone, Done: stopReasonToDone(evt.Delta.StopReason), RefusalCategory: evt.Delta.StopDetails.Category,
			})
		case "message_stop":
			s.closed = true
			_ = s.body.Close()
		case "error":
			_ = s.body.Close()
			return provider.Chunk{}, false, classifyAPIError(0, errorBody{Error: evt.Error})
		case "ping":
			// nothing to do
		}
	}
}

func stopReasonToDone(reason string) provider.DoneReason {
	switch reason {
	case "max_tokens":
		return provider.DoneMaxOutput
	case "refusal":
		// A policy decline (README task 13.7, closing F7) — never folded
		// into DoneStop: a safety refusal must not read as a clean
		// completion with empty content.
		return provider.DoneRefusal
	case "":
		return provider.DoneStop
	default: // "end_turn", "stop_sequence", "tool_use" all end this call normally
		return provider.DoneStop
	}
}

// readSSEFrame reads one "event: ...\ndata: ...\n\n" frame. Anthropic's SSE
// stream sends exactly one data line per event; a comment line (starting
// with ':') is skipped, matching the SSE spec.
func readSSEFrame(scanner *bufio.Scanner) (eventType string, data []byte, ok bool, err error) {
	var dataBuf bytes.Buffer
	sawAny := false
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if sawAny {
				return eventType, dataBuf.Bytes(), true, nil
			}
			continue // blank line before any field: keep reading
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			sawAny = true
		case strings.HasPrefix(line, "data:"):
			dataBuf.WriteString(strings.TrimPrefix(line, "data:"))
			sawAny = true
		}
	}
	if err := scanner.Err(); err != nil {
		return "", nil, false, err
	}
	if sawAny {
		return eventType, dataBuf.Bytes(), true, nil
	}
	return "", nil, false, nil
}
