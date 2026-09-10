// Package litellm is an OpenAI-Chat-Completions-compatible adapter against
// internal/provider.Provider (docs/local-llm.md) — plain net/http and SSE
// parsing, no vendor SDK, the same discipline internal/provider/anthropic
// already follows for the same reason (constitution Principle VII: "all
// provider access MUST go through one internal abstraction"). It talks to
// whatever speaks the OpenAI wire protocol: LiteLLM's proxy in front of a
// local Ollama model is the intended target (docs/local-llm.md), but nothing
// here is LiteLLM-specific — Ollama's own OpenAI-compatible endpoint works
// too. It is never exercised by a correctness test (Principle IX: those run
// only against internal/provider/fake); litellm_test.go proves the SSE
// parsing against a canned httptest.Server body, not a live call.
package litellm

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

// defaultBaseURL matches deploy/docker-compose.yml's litellm service, which
// publishes on host port 4100 (not LiteLLM's own default 4000 — confirmed
// colliding with another common local dev use of that port, docs/local-llm.md).
const defaultBaseURL = "http://localhost:4100"

// Provider implements provider.Provider against POST /v1/chat/completions
// with stream: true. One Provider value is bound to one model — exactly
// anthropic.Provider's own shape, and for the same reason: model selection
// across calls is internal/provider/router.go's job in principle, but this
// demo's Kernel only ever holds one process-wide Provider (kernel/loop.go
// never threads a per-call model id through provider.RunContext), so a
// bound model is what every adapter here provides.
type Provider struct {
	BaseURL string
	Model   string
	APIKey  string       // optional; empty is fine for a local, unauthenticated proxy
	Client  *http.Client // defaults to http.DefaultClient
}

func New(baseURL, model, apiKey string) *Provider {
	return &Provider{BaseURL: baseURL, Model: model, APIKey: apiKey}
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

// --- request shapes (the subset of the Chat Completions API this adapter uses) ---

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	Tools         []chatTool     `json:"tools,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

// streamOptions.IncludeUsage asks for the extra terminal chunk (empty
// choices, usage populated) the OpenAI wire protocol sends right before
// "[DONE]" — without it, a streaming call reports no usage at all.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string         `json:"role"` // system | user | assistant | tool
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`   // assistant turn issuing calls
	ToolCallID string         `json:"tool_call_id,omitempty"` // tool-result turn
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function chatToolCallFunc `json:"function"`
}

type chatToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded, as text
}

type chatTool struct {
	Type     string           `json:"type"` // always "function"
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// toChatMessages projects provider.Prompt onto the Chat Completions API's
// flat role-message shape. Two structural differences from
// anthropic.toAnthMessages, both because the two wire protocols disagree
// here, not because either translation is wrong: a "tool" role turn becomes
// ONE chatMessage PER BlockToolResult (OpenAI wants one message per result,
// where Anthropic wants one user turn carrying several blocks), and
// provider.BlockThinking is dropped — the OpenAI/Ollama wire has no
// equivalent slot for it, so a transcript that previously ran against
// Anthropic and picked up a thinking block simply loses it here rather than
// erroring; qwen2.5:3b has no extended-thinking mode to receive it anyway.
func toChatMessages(system string, msgs []provider.Message) []chatMessage {
	out := make([]chatMessage, 0, len(msgs)+1)
	if system != "" {
		out = append(out, chatMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			for _, b := range m.Blocks {
				if b.Kind == provider.BlockToolResult {
					out = append(out, chatMessage{Role: "tool", Content: b.Text, ToolCallID: b.ToolUseID})
				}
			}
		case "assistant":
			var text strings.Builder
			var calls []chatToolCall
			for _, b := range m.Blocks {
				switch b.Kind { //nolint:exhaustive // deliberately narrow: an assistant turn only ever carries text/tool_use blocks — BlockToolResult belongs to a "tool" role message (handled above), and BlockThinking is dropped on purpose (this doc comment's own paragraph)
				case provider.BlockText:
					text.WriteString(b.Text)
				case provider.BlockToolUse:
					calls = append(calls, chatToolCall{
						ID: b.ToolUseID, Type: "function",
						Function: chatToolCallFunc{Name: b.ToolName, Arguments: string(b.Input)},
					})
				}
			}
			out = append(out, chatMessage{Role: "assistant", Content: text.String(), ToolCalls: calls})
		default: // "user"
			out = append(out, chatMessage{Role: m.Role, Content: m.PlainText()})
		}
	}
	return out
}

func toChatTools(schemas []provider.ToolSchema) []chatTool {
	if len(schemas) == 0 {
		return nil
	}
	out := make([]chatTool, 0, len(schemas))
	for _, s := range schemas {
		out = append(out, chatTool{Type: "function", Function: chatToolFunction{
			Name: s.Name, Description: s.Description, Parameters: s.InputSchema,
		}})
	}
	return out
}

// --- error shapes ---

type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// classifyAPIError mirrors anthropic.classifyAPIError: turn a wire-level
// error into the typed sentinel internal/provider/failover.go's
// ClassifyTrigger already knows how to read.
func classifyAPIError(status int, body errorBody) error {
	msg := body.Error.Message
	if msg == "" {
		msg = fmt.Sprintf("http %d", status)
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "context") && (strings.Contains(lower, "length") || strings.Contains(lower, "too long") || strings.Contains(lower, "maximum")):
		return &provider.ContextOverflowError{Reason: msg}
	case status == http.StatusTooManyRequests, status >= 500:
		return &provider.ThrottleError{Reason: msg}
	default:
		return fmt.Errorf("litellm: %s: %s", body.Error.Type, msg)
	}
}

// Stream issues the request and returns a normalized Stream. An error
// returned here (never via the Stream it would have returned) means the
// endpoint refused the call outright — mirrors provider.ThrottleError's
// documented contract, same as anthropic.Provider.Stream.
func (p *Provider) Stream(ctx context.Context, prompt provider.Prompt, tools []provider.ToolSchema, _ provider.RunContext) (provider.Stream, error) {
	reqBody := chatRequest{
		Model:         p.Model,
		Messages:      toChatMessages(prompt.System, prompt.Messages),
		Tools:         toChatTools(tools),
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("litellm: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL()+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("litellm: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("authorization", "Bearer "+p.APIKey)
	}

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

	// Same 1MB line cap as anthropic.go, same reason (README task 13.14): a
	// single content/tool-argument delta can exceed bufio.Scanner's 64KB
	// default.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	return &stream{body: resp.Body, scanner: scanner, toolCalls: map[int]*toolCallState{}}, nil
}

type toolCallState struct {
	id, name string
	args     bytes.Buffer
}

// chatStreamChunk is one SSE "data:" line's decoded shape. Usage is only
// non-nil on the terminal chunk stream_options.include_usage asked for,
// which some backends attach to the SAME chunk as the finish_reason and
// others send as a separate, later chunk with an empty Choices — stream.Next
// handles either ordering by finalizing on the literal "[DONE]" line rather
// than assuming a fixed chunk shape.
type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

type stream struct {
	body      io.ReadCloser
	scanner   *bufio.Scanner
	toolCalls map[int]*toolCallState

	usage      provider.Usage
	usageSeen  bool
	gotDone    bool // a finish_reason chunk has been seen
	doneReason provider.DoneReason

	pending []provider.Chunk // FIFO of chunks decoded from one SSE line, drained before reading the next
	closed  bool
}

// Next decodes one SSE "data:" line at a time, translating it into zero or
// more normalized Chunks — a tool_calls delta accumulates silently by index
// until the finish_reason chunk, mirroring anthropic.go's
// content_block_stop-triggered flush. Usage and Done are only emitted once
// the literal "[DONE]" line arrives, so whichever chunk (finish_reason's own
// or a later empty-choices one) actually carries usage doesn't matter.
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

		line, ok, err := readSSELine(s.scanner)
		if err != nil {
			return provider.Chunk{}, false, fmt.Errorf("litellm: read stream: %w", err)
		}
		if !ok {
			// Clean EOF without a "[DONE]" line — the contract callers rely
			// on (provider.Stream's doc comment) is that ChunkDone is
			// emitted before a clean end on any non-error path; a stream
			// that ends here without one is truncated.
			_ = s.body.Close()
			return provider.Chunk{}, false, io.ErrUnexpectedEOF
		}

		if line == "[DONE]" {
			s.closed = true
			_ = s.body.Close()
			if !s.gotDone {
				return provider.Chunk{}, false, io.ErrUnexpectedEOF
			}
			s.pending = append(s.pending,
				provider.Chunk{Kind: provider.ChunkUsage, Usage: s.usage},
				provider.Chunk{Kind: provider.ChunkDone, Done: s.doneReason},
			)
			continue
		}

		var evt chatStreamChunk
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			return provider.Chunk{}, false, fmt.Errorf("litellm: parse chunk: %w", err)
		}

		if evt.Usage != nil {
			cacheRead := 0
			if evt.Usage.PromptTokensDetails != nil {
				cacheRead = evt.Usage.PromptTokensDetails.CachedTokens
			}
			s.usage = provider.Usage{
				InputUncached:  evt.Usage.PromptTokens - cacheRead,
				InputCacheRead: cacheRead,
				OutputTokens:   evt.Usage.CompletionTokens,
			}
			s.usageSeen = true
		}

		if len(evt.Choices) == 0 {
			continue
		}
		choice := evt.Choices[0]
		if choice.Delta.Content != "" {
			s.pending = append(s.pending, provider.Chunk{Kind: provider.ChunkContent, Text: choice.Delta.Content})
		}
		for _, tc := range choice.Delta.ToolCalls {
			tb, ok := s.toolCalls[tc.Index]
			if !ok {
				tb = &toolCallState{}
				s.toolCalls[tc.Index] = tb
			}
			if tc.ID != "" {
				tb.id = tc.ID
			}
			if tc.Function.Name != "" {
				tb.name = tc.Function.Name
			}
			tb.args.WriteString(tc.Function.Arguments)
		}
		if choice.FinishReason != nil {
			s.gotDone = true
			s.doneReason = finishReasonToDone(*choice.FinishReason)
			s.flushToolCalls()
		}
	}
}

// flushToolCalls emits one ChunkToolUse per accumulated call, in index
// order — the finish_reason chunk is this wire protocol's equivalent of
// anthropic.go's per-block content_block_stop, just covering every call at
// once instead of one block at a time (the Chat Completions API has no
// per-call stop signal).
func (s *stream) flushToolCalls() {
	for i := 0; i < len(s.toolCalls); i++ {
		tb, ok := s.toolCalls[i]
		if !ok {
			continue
		}
		args := tb.args.Bytes()
		if len(args) == 0 {
			args = []byte("{}")
		}
		s.pending = append(s.pending, provider.Chunk{
			Kind: provider.ChunkToolUse, ToolUseID: tb.id, ToolName: tb.name, Input: json.RawMessage(args),
		})
	}
	s.toolCalls = map[int]*toolCallState{}
}

func finishReasonToDone(reason string) provider.DoneReason {
	switch reason {
	case "length":
		return provider.DoneMaxOutput
	case "content_filter":
		return provider.DoneRefusal
	default: // "stop", "tool_calls", "function_call" all end this call normally
		return provider.DoneStop
	}
}

// readSSELine returns the next non-empty "data:" line's payload, skipping
// blank lines and ":"-prefixed comment lines per the SSE spec — simpler than
// anthropic.go's readSSEFrame because this wire protocol has no "event:"
// line, only "data: ...".
func readSSELine(scanner *bufio.Scanner) (data string, ok bool, err error) {
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "", strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "data:"):
			return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", false, err
	}
	return "", false, nil
}
