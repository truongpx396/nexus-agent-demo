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
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []anthMessage `json:"messages"`
	Tools     []anthTool    `json:"tools,omitempty"`
	Stream    bool          `json:"stream"`
}

type anthMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// toAnthMessages projects the kernel's plain-text transcript onto the
// Messages API's role-content shape. provider.Message has no structured
// tool_result content block (internal/provider.Prompt is deliberately
// minimal — see its doc comment); a "tool" role rides as a user-turn text
// message with the pairing made explicit in the text itself. Native
// structured tool_result blocks are a future adapter refinement, not a
// Phase 2 task.
func toAnthMessages(msgs []provider.Message) []anthMessage {
	out := make([]anthMessage, 0, len(msgs))
	for _, m := range msgs {
		role := m.Role
		text := m.Text
		if role == "tool" {
			role = "user"
			text = "[tool_result] " + text
		}
		out = append(out, anthMessage{Role: role, Content: text})
	}
	return out
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
		StopReason  string `json:"stop_reason"`
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
		System:    prompt.System,
		Messages:  toAnthMessages(prompt.Messages),
		Tools:     toAnthTools(tools),
		Stream:    true,
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

	return &stream{body: resp.Body, scanner: bufio.NewScanner(resp.Body), toolBlocks: map[int]*toolBlockState{}}, nil
}

type toolBlockState struct {
	id, name string
	json     bytes.Buffer
}

type stream struct {
	body       io.ReadCloser
	scanner    *bufio.Scanner
	toolBlocks map[int]*toolBlockState
	usage      provider.Usage
	pending    []provider.Chunk // FIFO of chunks decoded from one SSE frame, drained before reading the next frame
	closed     bool
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
			if evt.ContentBlock.Type == "tool_use" {
				s.toolBlocks[evt.Index] = &toolBlockState{id: evt.ContentBlock.ID, name: evt.ContentBlock.Name}
			}
		case "content_block_delta":
			switch evt.Delta.Type {
			case "text_delta":
				s.pending = append(s.pending, provider.Chunk{Kind: provider.ChunkContent, Text: evt.Delta.Text})
			case "input_json_delta":
				if tb, ok := s.toolBlocks[evt.Index]; ok {
					tb.json.WriteString(evt.Delta.PartialJSON)
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
		case "message_delta":
			s.usage.OutputTokens = evt.Usage.OutputTokens
			s.pending = append(s.pending, provider.Chunk{Kind: provider.ChunkUsage, Usage: s.usage})
			s.pending = append(s.pending, provider.Chunk{Kind: provider.ChunkDone, Done: stopReasonToDone(evt.Delta.StopReason)})
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
