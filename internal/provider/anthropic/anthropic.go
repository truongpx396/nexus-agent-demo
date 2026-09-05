// Package anthropic is the one real hosted-model adapter this demo ships
// (README task 2.8) against internal/provider.Provider — plain net/http and
// SSE parsing, no vendor SDK, because "all provider access MUST go through
// one internal abstraction" (constitution Principle VII) means the wire
// protocol lives here and nowhere else. It is never exercised by a
// correctness test (Principle IX: those run only against
// internal/provider/fake); anthropic_test.go proves the SSE parsing against
// a canned httptest.Server body, not a live call.
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

const (
	defaultBaseURL   = "https://api.anthropic.com"
	apiVersion       = "2023-06-01"
	defaultMaxTokens = 4096
)

// Provider implements provider.Provider against POST /v1/messages with
// stream: true. One Provider value is bound to one model — model selection
// across calls is internal/provider/router.go's job (README task 2.8), not
// this adapter's.
type Provider struct {
	APIKey    string
	Model     string
	BaseURL   string       // defaults to defaultBaseURL
	Client    *http.Client // defaults to http.DefaultClient
	MaxTokens int          // defaults to defaultMaxTokens
}

func New(apiKey, model string) *Provider {
	return &Provider{APIKey: apiKey, Model: model}
}

func (p *Provider) baseURL() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return defaultBaseURL
}

func (p *Provider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

func (p *Provider) maxTokens() int {
	if p.MaxTokens != 0 {
		return p.MaxTokens
	}
	return defaultMaxTokens
}

// --- request shapes (the subset of the Messages API this adapter uses) ---

type messagesRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    []anthTextBlock `json:"system,omitempty"`
	Messages  []anthMessage   `json:"messages"`
	Tools     []anthTool      `json:"tools,omitempty"`
	Stream    bool            `json:"stream"`
	Thinking  *anthThinking   `json:"thinking,omitempty"`
}

// anthTextBlock is System's element shape (README task 13.5, closing F4):
// one block with an ephemeral cache_control breakpoint on it caches
// everything before and including it — tools -> system -> messages is the
// API's render order, and Tools is already deterministically sorted
// (internal/tools/manifest.go), so one breakpoint here covers both the
// stable tool catalog and the stable system prompt.
type anthTextBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	CacheControl *anthCacheControl `json:"cache_control,omitempty"`
}

type anthCacheControl struct {
	Type string `json:"type"`
}

// anthThinking requests extended/adaptive thinking (README task 13.7,
// closing F7). Only {"type":"adaptive"} is ever sent by this adapter —
// thinkingConfigFor decides which models get it.
type anthThinking struct {
	Type string `json:"type"`
}

type anthMessage struct {
	Role    string             `json:"role"`
	Content []anthContentBlock `json:"content"`
}

// anthContentBlock is the Anthropic Messages API's native content-block
// shape (README task 13.4, closing F5) — which fields are populated depends
// on Type, mirroring provider.ContentBlock one-for-one.
type anthContentBlock struct {
	Type string `json:"type"`

	Text      string `json:"text,omitempty"`      // text
	Thinking  string `json:"thinking,omitempty"`  // thinking
	Signature string `json:"signature,omitempty"` // thinking — echoed back verbatim

	ID    string          `json:"id,omitempty"`    // tool_use
	Name  string          `json:"name,omitempty"`  // tool_use
	Input json.RawMessage `json:"input,omitempty"` // tool_use

	ToolUseID string `json:"tool_use_id,omitempty"` // tool_result
	Content   string `json:"content,omitempty"`     // tool_result
	IsError   bool   `json:"is_error,omitempty"`    // tool_result
}

type anthTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// toAnthMessages projects provider.Message's typed content blocks onto the
// Messages API's native role-content-blocks shape (README task 13.4,
// closing F5) — a "tool" role always rides as a user-turn message carrying
// tool_result blocks, exactly what the API expects; every other kind maps
// 1:1 onto its Anthropic block type, preserving tool_use_id pairing and a
// thinking block's signature all the way to the wire.
func toAnthMessages(msgs []provider.Message) []anthMessage {
	out := make([]anthMessage, 0, len(msgs))
	for _, m := range msgs {
		role := m.Role
		if role == "tool" {
			role = "user"
		}
		blocks := make([]anthContentBlock, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			switch b.Kind {
			case provider.BlockText:
				blocks = append(blocks, anthContentBlock{Type: "text", Text: b.Text})
			case provider.BlockThinking:
				blocks = append(blocks, anthContentBlock{Type: "thinking", Thinking: b.Text, Signature: b.Signature})
			case provider.BlockToolUse:
				blocks = append(blocks, anthContentBlock{Type: "tool_use", ID: b.ToolUseID, Name: b.ToolName, Input: b.Input})
			case provider.BlockToolResult:
				blocks = append(blocks, anthContentBlock{Type: "tool_result", ToolUseID: b.ToolUseID, Content: b.Text, IsError: b.IsError})
			}
		}
		out = append(out, anthMessage{Role: role, Content: blocks})
	}
	return out
}

// toAnthSystem wraps the stable system prompt in a single block with an
// ephemeral cache_control breakpoint (README task 13.5, closing F4) — the
// wire-level half of the two-zone byte-stable prefix internal/promptctx
// already builds; an empty system prompt sends no block at all (a
// zero-length cached block is meaningless and some callers, e.g. the
// condenser prompt, pass no system text).
func toAnthSystem(system string) []anthTextBlock {
	if system == "" {
		return nil
	}
	return []anthTextBlock{{Type: "text", Text: system, CacheControl: &anthCacheControl{Type: "ephemeral"}}}
}

// thinkingConfigFor decides which models get adaptive thinking (README task
// 13.7, closing F7). claude-sonnet-5 and claude-opus-5 both default to
// adaptive thinking already and reject the legacy budget_tokens shape, so
// this makes that explicit; claude-haiku-4-5 (and anything unrecognized)
// gets no thinking config at all — the safe default, since Haiku still
// requires a budget_tokens this adapter has no basis to guess.
func thinkingConfigFor(model string) *anthThinking {
	switch model {
	case "claude-sonnet-5", "claude-opus-5":
		return &anthThinking{Type: "adaptive"}
	default:
		return nil
	}
}

func toAnthTools(schemas []provider.ToolSchema) []anthTool {
	if len(schemas) == 0 {
		return nil
	}
	out := make([]anthTool, 0, len(schemas))
	for _, s := range schemas {
		out = append(out, anthTool{Name: s.Name, Description: s.Description, InputSchema: s.InputSchema})
	}
	return out
}

// --- response / SSE event shapes ---

type sseUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

type sseEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage sseUsage `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
		StopDetails struct {
			Category string `json:"category"`
		} `json:"stop_details"`
	} `json:"delta"`
	Usage sseUsage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type errorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// classifyAPIError turns an Anthropic error payload into the typed error
// internal/provider/failover.go's ClassifyTrigger already knows how to read
// — this adapter doesn't reimplement the taxonomy, it just produces the
// right sentinel type at the boundary.
func classifyAPIError(status int, body errorBody) error {
	msg := body.Error.Message
	if msg == "" {
		msg = fmt.Sprintf("http %d", status)
	}
	switch {
	case strings.Contains(strings.ToLower(msg), "too long") || strings.Contains(strings.ToLower(msg), "context"):
		return &provider.ContextOverflowError{Reason: msg}
	case status == http.StatusTooManyRequests, status >= 500, body.Error.Type == "overloaded_error", body.Error.Type == "rate_limit_error":
		return &provider.ThrottleError{Reason: msg}
	default:
		return fmt.Errorf("anthropic: %s: %s", body.Error.Type, msg)
	}
}

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
