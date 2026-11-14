package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/truongpx396/nexus-agent/internal/provider"
)

// canned is a realistic Messages API SSE body: text content, then a tool
// call, then usage + a natural stop — fed through httptest.Server so this
// test proves the parsing deterministically, never calling the live API
// (constitution Principle IX).
const canned = "event: message_start\n" +
	`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"nyc\"}"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func newTestServer(t *testing.T, status int, body string, checkReq func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if checkReq != nil {
			checkReq(r)
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestStreamParsesTextToolUseUsageAndDone(t *testing.T) {
	var gotAPIKey, gotVersion string
	srv := newTestServer(t, http.StatusOK, canned, func(r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
	})
	defer srv.Close()

	p := &Provider{APIKey: "test-key", Model: "claude-test", BaseURL: srv.URL}
	stream, err := p.Stream(context.Background(), provider.Prompt{System: "sys", Messages: []provider.Message{provider.TextMessage("user", "hi")}}, nil, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var kinds []provider.ChunkKind
	var text string
	var toolUse provider.Chunk
	var usage provider.Usage
	var done provider.DoneReason
	for {
		c, ok, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		kinds = append(kinds, c.Kind)
		switch c.Kind {
		case provider.ChunkContent:
			text += c.Text
		case provider.ChunkToolUse:
			toolUse = c
		case provider.ChunkUsage:
			usage = c.Usage
		case provider.ChunkDone:
			done = c.Done
		case provider.ChunkReasoning:
			// not produced by this fixture
		}
	}

	if gotAPIKey != "test-key" {
		t.Fatalf("x-api-key = %q", gotAPIKey)
	}
	if gotVersion != apiVersion {
		t.Fatalf("anthropic-version = %q, want %q", gotVersion, apiVersion)
	}
	if text != "Hello world" {
		t.Fatalf("text = %q, want %q", text, "Hello world")
	}
	if toolUse.ToolUseID != "toolu_1" || toolUse.ToolName != "get_weather" || string(toolUse.Input) != `{"city":"nyc"}` {
		t.Fatalf("tool use chunk = %+v", toolUse)
	}
	if usage.InputUncached != 50 || usage.OutputTokens != 12 {
		t.Fatalf("usage = %+v", usage)
	}
	if done != provider.DoneStop {
		t.Fatalf("done = %q, want stop", done)
	}
	wantKinds := []provider.ChunkKind{
		provider.ChunkContent, provider.ChunkContent, provider.ChunkToolUse, provider.ChunkUsage, provider.ChunkDone,
	}
	if len(kinds) != len(wantKinds) {
		t.Fatalf("kinds = %v, want %v", kinds, wantKinds)
	}
	for i := range wantKinds {
		if kinds[i] != wantKinds[i] {
			t.Fatalf("kinds[%d] = %q, want %q (full: %v)", i, kinds[i], wantKinds[i], kinds)
		}
	}
}

func TestStreamMaxTokensMapsToDoneMaxOutput(t *testing.T) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":4096}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	srv := newTestServer(t, http.StatusOK, body, nil)
	defer srv.Close()

	p := &Provider{APIKey: "k", Model: "m", BaseURL: srv.URL}
	stream, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var done provider.DoneReason
	for {
		c, ok, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		if c.Kind == provider.ChunkDone {
			done = c.Done
		}
	}
	if done != provider.DoneMaxOutput {
		t.Fatalf("done = %q, want max_output", done)
	}
}

func TestStreamRateLimitIsThrottleError(t *testing.T) {
	srv := newTestServer(t, http.StatusTooManyRequests,
		`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`, nil)
	defer srv.Close()

	p := &Provider{APIKey: "k", Model: "m", BaseURL: srv.URL}
	_, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if provider.ClassifyTrigger(err) != provider.TriggerRetryable {
		t.Fatalf("ClassifyTrigger(%v) = %v, want retryable", err, provider.ClassifyTrigger(err))
	}
}

func TestStreamContextOverflowIsClassified(t *testing.T) {
	srv := newTestServer(t, http.StatusBadRequest,
		`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}}`, nil)
	defer srv.Close()

	p := &Provider{APIKey: "k", Model: "m", BaseURL: srv.URL}
	_, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if provider.ClassifyTrigger(err) != provider.TriggerContextOverflow {
		t.Fatalf("ClassifyTrigger(%v) = %v, want context_overflow", err, provider.ClassifyTrigger(err))
	}
}

// TestStreamRequestBody_CachesSystemAndSetsThinking is README task 13.5/13.7
// (closing F4/F7): the request body actually sent on the wire must carry an
// ephemeral cache_control breakpoint on the system block, and an adaptive
// thinking config for a model that supports it.
func TestStreamRequestBody_CachesSystemAndSetsThinking(t *testing.T) {
	var gotBody []byte
	srv := newTestServer(t, http.StatusOK, canned, func(r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
	})
	defer srv.Close()

	p := &Provider{APIKey: "k", Model: "claude-sonnet-5", BaseURL: srv.URL}
	// The returned stream is never drained here and provider.Stream has no Close,
	// so cancelling the context is what releases the response body's connection.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := p.Stream(ctx, provider.Prompt{System: "be helpful"}, nil, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var req messagesRequest
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("unmarshal sent request body: %v (body=%s)", err, gotBody)
	}
	if len(req.System) != 1 || req.System[0].Text != "be helpful" {
		t.Fatalf("System = %+v, want one block with the system text", req.System)
	}
	if req.System[0].CacheControl == nil || req.System[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("System[0].CacheControl = %+v, want an ephemeral breakpoint", req.System[0].CacheControl)
	}
	if req.Thinking == nil || req.Thinking.Type != "adaptive" {
		t.Fatalf("Thinking = %+v, want adaptive for claude-sonnet-5", req.Thinking)
	}
}

func TestThinkingConfigFor(t *testing.T) {
	cases := map[string]bool{
		"claude-sonnet-5":  true,
		"claude-opus-5":    true,
		"claude-haiku-4-5": false,
		"unknown-model":    false,
	}
	for model, wantThinking := range cases {
		got := thinkingConfigFor(model)
		if wantThinking && (got == nil || got.Type != "adaptive") {
			t.Errorf("thinkingConfigFor(%q) = %+v, want adaptive", model, got)
		}
		if !wantThinking && got != nil {
			t.Errorf("thinkingConfigFor(%q) = %+v, want nil", model, got)
		}
	}
}

// TestStreamThinkingBlockRoundTrips is README task 13.7: thinking_delta and
// signature_delta accumulate per content-block index and are emitted as one
// ChunkReasoning whose Opaque decodes back to the same thinking/signature —
// what kernel/rehydrate.go's reasoningOpaque later reconstructs from.
func TestStreamThinkingBlockRoundTrips(t *testing.T) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	srv := newTestServer(t, http.StatusOK, body, nil)
	defer srv.Close()

	p := &Provider{APIKey: "k", Model: "m", BaseURL: srv.URL}
	stream, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var gotOpaque []byte
	for {
		c, ok, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		if c.Kind == provider.ChunkReasoning {
			gotOpaque = c.Opaque
		}
	}
	if gotOpaque == nil {
		t.Fatal("no ChunkReasoning chunk was emitted")
	}
	var decoded thinkingOpaque
	if err := json.Unmarshal(gotOpaque, &decoded); err != nil {
		t.Fatalf("unmarshal thinking opaque: %v", err)
	}
	if decoded.Thinking != "let me think" || decoded.Signature != "sig-abc" {
		t.Fatalf("decoded thinking opaque = %+v, want {let me think, sig-abc}", decoded)
	}
}

// TestStreamRefusalMapsToDoneRefusal is README task 13.7 (closing F7): a
// refusal stop_reason must never fold into DoneStop, and stop_details.category
// must survive onto the ChunkDone chunk.
func TestStreamRefusalMapsToDoneRefusal(t *testing.T) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"category":"cyber"}},"usage":{"output_tokens":0}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	srv := newTestServer(t, http.StatusOK, body, nil)
	defer srv.Close()

	p := &Provider{APIKey: "k", Model: "m", BaseURL: srv.URL}
	stream, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var done provider.DoneReason
	var category string
	for {
		c, ok, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		if c.Kind == provider.ChunkDone {
			done = c.Done
			category = c.RefusalCategory
		}
	}
	if done != provider.DoneRefusal {
		t.Fatalf("done = %q, want refusal", done)
	}
	if category != "cyber" {
		t.Fatalf("RefusalCategory = %q, want cyber", category)
	}
}

func TestStreamInvalidRequestIsPermanent(t *testing.T) {
	srv := newTestServer(t, http.StatusBadRequest,
		`{"type":"error","error":{"type":"invalid_request_error","message":"model not found"}}`, nil)
	defer srv.Close()

	p := &Provider{APIKey: "k", Model: "m", BaseURL: srv.URL}
	_, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if provider.ClassifyTrigger(err) != provider.TriggerPermanent {
		t.Fatalf("ClassifyTrigger(%v) = %v, want permanent", err, provider.ClassifyTrigger(err))
	}
}

// TestStreamSendsWireSafeToolNames is the R1 regression: the Messages API
// rejects a tool name outside [a-zA-Z0-9_-]{1,128}, so the internal reference
// `platform/shell@v1` must reach the wire as `platform_shell_v1` — in the tool
// list and in a replayed tool_use — and a tool_use block the model names that
// way must come back to the kernel under the internal reference.
func TestStreamSendsWireSafeToolNames(t *testing.T) {
	const ref = "platform/shell@v1"
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"platform_shell_v1"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	var gotBody []byte
	srv := newTestServer(t, http.StatusOK, body, func(r *http.Request) { gotBody, _ = io.ReadAll(r.Body) })
	defer srv.Close()

	prompt := provider.Prompt{Messages: []provider.Message{
		provider.TextMessage("user", "list files"),
		{Role: "assistant", Blocks: []provider.ContentBlock{
			{Kind: provider.BlockToolUse, ToolUseID: "toolu_1", ToolName: ref, Input: json.RawMessage(`{"cmd":"pwd"}`)},
		}},
		provider.ToolResultMessage("toolu_1", "/work", false),
	}}
	tools := []provider.ToolSchema{{Name: ref, Description: "run a command", InputSchema: json.RawMessage(`{"type":"object"}`)}}

	p := &Provider{APIKey: "k", Model: "m", BaseURL: srv.URL}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := p.Stream(ctx, prompt, tools, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var req struct {
		Tools    []struct{ Name string } `json:"tools"`
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "platform_shell_v1" {
		t.Fatalf("tools = %+v, want one tool named platform_shell_v1", req.Tools)
	}
	if got := req.Messages[1].Content[0]; got.Type != "tool_use" || got.Name != "platform_shell_v1" {
		t.Fatalf("replayed tool_use = %+v, want name platform_shell_v1", got)
	}

	for {
		c, ok, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			t.Fatal("stream ended without a tool_use chunk")
		}
		if c.Kind == provider.ChunkToolUse {
			if c.ToolName != ref {
				t.Fatalf("ChunkToolUse.ToolName = %q, want the internal reference %q", c.ToolName, ref)
			}
			return
		}
	}
}

// Two references that sanitize to one wire name cannot be told apart in the
// model's reply, so the call is refused before anything is sent.
func TestStreamRefusesCollidingToolNames(t *testing.T) {
	srv := newTestServer(t, http.StatusOK, canned, func(*http.Request) { t.Error("a request was sent") })
	defer srv.Close()
	p := &Provider{APIKey: "k", Model: "m", BaseURL: srv.URL}
	_, err := p.Stream(context.Background(), provider.Prompt{}, []provider.ToolSchema{{Name: "a/b@v1"}, {Name: "a_b_v1"}}, provider.RunContext{})
	if !errors.Is(err, provider.ErrWireNameCollision) {
		t.Fatalf("err = %v, want ErrWireNameCollision", err)
	}
}

// TestStreamSendsTheConfiguredMaxTokens: the output limit is a setting
// (NEXUS_ANTHROPIC_MAX_TOKENS), not the adapter's fixed 4096.
func TestStreamSendsTheConfiguredMaxTokens(t *testing.T) {
	for name, tc := range map[string]struct {
		maxTokens int
		want      int
	}{
		"configured": {16000, 16000},
		"unset":      {0, defaultMaxTokens},
	} {
		t.Run(name, func(t *testing.T) {
			var gotBody []byte
			srv := newTestServer(t, http.StatusOK, canned, func(r *http.Request) { gotBody, _ = io.ReadAll(r.Body) })
			defer srv.Close()
			p := &Provider{APIKey: "k", Model: "m", BaseURL: srv.URL, MaxTokens: tc.maxTokens}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if _, err := p.Stream(ctx, provider.Prompt{}, nil, provider.RunContext{}); err != nil {
				t.Fatalf("Stream: %v", err)
			}
			var req struct {
				MaxTokens int `json:"max_tokens"`
			}
			if err := json.Unmarshal(gotBody, &req); err != nil {
				t.Fatal(err)
			}
			if req.MaxTokens != tc.want {
				t.Fatalf("max_tokens = %d, want %d", req.MaxTokens, tc.want)
			}
		})
	}
}

// A 429 carries the provider's Retry-After, which the retry wrapper honours.
func TestStreamThrottleCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("retry-after", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer srv.Close()

	p := &Provider{APIKey: "k", Model: "m", BaseURL: srv.URL}
	_, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	te, ok := errors.AsType[*provider.ThrottleError](err)
	if !ok {
		t.Fatalf("err = %v, want a ThrottleError", err)
	}
	if te.RetryAfter != 7*time.Second {
		t.Fatalf("RetryAfter = %v, want 7s", te.RetryAfter)
	}
}

// A run cancelled mid-request is the caller's doing; reporting it as a throttle
// would have the retry layer start the call again.
func TestStreamCancelledContextIsNotClassifiedRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &Provider{APIKey: "k", Model: "m", BaseURL: srv.URL}
	_, err := p.Stream(ctx, provider.Prompt{}, nil, provider.RunContext{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := provider.ClassifyTrigger(err); got == provider.TriggerRetryable {
		t.Fatalf("a cancelled call classified as %s", got)
	}
}

// TestContentBlocksSerialiseWithExactlyTheirOwnFields is R5: one struct with
// omitempty on every field dropped the fields the API requires whenever they were
// empty. A thinking block that comes back with a signature but no text must still
// carry "thinking", and a tool_use with no input must still carry "input".
func TestContentBlocksSerialiseWithExactlyTheirOwnFields(t *testing.T) {
	for name, tc := range map[string]struct {
		block provider.ContentBlock
		want  string
	}{
		"text": {
			provider.ContentBlock{Kind: provider.BlockText, Text: "hi"},
			`{"type":"text","text":"hi"}`,
		},
		"thinking with its text": {
			provider.ContentBlock{Kind: provider.BlockThinking, Text: "hmm", Signature: "sig"},
			`{"type":"thinking","thinking":"hmm","signature":"sig"}`,
		},
		"thinking with empty text keeps the field": {
			provider.ContentBlock{Kind: provider.BlockThinking, Signature: "sig"},
			`{"type":"thinking","thinking":"","signature":"sig"}`,
		},
		"tool_use": {
			provider.ContentBlock{Kind: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "get_weather", Input: json.RawMessage(`{"city":"nyc"}`)},
			`{"type":"tool_use","id":"tu1","name":"get_weather","input":{"city":"nyc"}}`,
		},
		"tool_use with nil input sends an empty object": {
			provider.ContentBlock{Kind: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "ping"},
			`{"type":"tool_use","id":"tu1","name":"ping","input":{}}`,
		},
		"tool_use with empty input sends an empty object": {
			provider.ContentBlock{Kind: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "ping", Input: json.RawMessage{}},
			`{"type":"tool_use","id":"tu1","name":"ping","input":{}}`,
		},
		"tool_result": {
			provider.ContentBlock{Kind: provider.BlockToolResult, ToolUseID: "tu1", Text: "sunny"},
			`{"type":"tool_result","tool_use_id":"tu1","content":"sunny"}`,
		},
		"tool_result error": {
			provider.ContentBlock{Kind: provider.BlockToolResult, ToolUseID: "tu1", Text: "boom", IsError: true},
			`{"type":"tool_result","tool_use_id":"tu1","content":"boom","is_error":true}`,
		},
		"tool_result with no content omits it": {
			provider.ContentBlock{Kind: provider.BlockToolResult, ToolUseID: "tu1"},
			`{"type":"tool_result","tool_use_id":"tu1"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			// A closing user message takes the history's cache breakpoint, so the block
			// under test is marshalled with only the fields its own type defines.
			msgs := toAnthMessages([]provider.Message{
				{Role: "assistant", Blocks: []provider.ContentBlock{tc.block}},
				provider.TextMessage("user", "end"),
			}, provider.WireNames{})
			if len(msgs) != 2 || len(msgs[0].Content) != 1 {
				t.Fatalf("messages = %+v", msgs)
			}
			got, err := json.Marshal(msgs[0].Content[0])
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("block = %s\nwant    %s", got, tc.want)
			}
		})
	}

	t.Run("an unknown type is an error", func(t *testing.T) {
		if _, err := json.Marshal(anthContentBlock{Type: "image"}); err == nil {
			t.Fatal("expected an error for a block type the adapter does not produce")
		}
	})

	t.Run("a whole request body round trips through its own decoder", func(t *testing.T) {
		raw, err := json.Marshal(messagesRequest{Model: "m", MaxTokens: 1, Messages: toAnthMessages([]provider.Message{
			provider.TextMessage("user", "go"),
			{Role: "assistant", Blocks: []provider.ContentBlock{
				{Kind: provider.BlockThinking, Signature: "sig"},
				{Kind: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "ping"},
			}},
			provider.ToolResultMessage("tu1", "pong", false),
		}, provider.WireNames{})})
		if err != nil {
			t.Fatal(err)
		}
		var back messagesRequest
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, raw)
		}
		if got := back.Messages[1].Content[0]; got.Type != "thinking" || got.Signature != "sig" {
			t.Fatalf("thinking block did not survive: %+v", got)
		}
		if got := string(back.Messages[1].Content[1].Input); got != "{}" {
			t.Fatalf("tool_use input = %q, want {}", got)
		}
	})
}

// TestStreamCachesTheConversationHistory is R8: the system block's breakpoint
// covers only the tools and the system prompt, so an agent loop, whose growing
// history is most of the input, was re-sent in full every turn. A second
// breakpoint on the last block makes each call read what the previous one wrote.
func TestStreamCachesTheConversationHistory(t *testing.T) {
	var gotBody []byte
	srv := newTestServer(t, http.StatusOK, canned, func(r *http.Request) { gotBody, _ = io.ReadAll(r.Body) })
	defer srv.Close()

	p := &Provider{APIKey: "k", Model: "m", BaseURL: srv.URL}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prompt := provider.Prompt{System: "sys", Messages: []provider.Message{
		provider.TextMessage("user", "go"),
		{Role: "assistant", Blocks: []provider.ContentBlock{
			{Kind: provider.BlockThinking, Signature: "sig"},
			{Kind: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "ping"},
		}},
		provider.ToolResultMessage("tu1", "pong", false),
	}}
	if _, err := p.Stream(ctx, prompt, nil, provider.RunContext{}); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var req struct {
		System []struct {
			CacheControl *anthCacheControl `json:"cache_control"`
		} `json:"system"`
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	breakpoints := 0
	if len(req.System) == 1 && req.System[0].CacheControl != nil {
		breakpoints++ // the system breakpoint stays
	}
	var onBlocks []string
	for mi, m := range req.Messages {
		for bi, b := range m.Content {
			if cc, ok := b["cache_control"]; ok {
				breakpoints++
				onBlocks = append(onBlocks, fmt.Sprintf("%v@%d.%d", b["type"], mi, bi))
				if cc.(map[string]any)["type"] != "ephemeral" {
					t.Errorf("cache_control = %v, want ephemeral", cc)
				}
			}
		}
	}
	if len(onBlocks) != 1 || onBlocks[0] != "tool_result@2.0" {
		t.Fatalf("history breakpoints on %v, want exactly the last block (the tool_result of the last message)", onBlocks)
	}
	if breakpoints != 2 || breakpoints > 4 {
		t.Fatalf("%d breakpoints in the request, want the system one and the history one (the API allows 4)", breakpoints)
	}
}

// A thinking block cannot carry a breakpoint, so the last block that can does.
func TestCacheLastBlockSkipsThinkingAndHandlesEmpty(t *testing.T) {
	cacheLastBlock(nil) // no messages: nothing to do, no panic

	msgs := toAnthMessages([]provider.Message{{Role: "assistant", Blocks: []provider.ContentBlock{
		{Kind: provider.BlockText, Text: "so"},
		{Kind: provider.BlockThinking, Text: "hmm", Signature: "sig"},
	}}}, provider.WireNames{})
	if msgs[0].Content[0].CacheControl == nil || msgs[0].Content[1].CacheControl != nil {
		t.Fatalf("breakpoint on %+v, want the text block and not the thinking block", msgs[0].Content)
	}
	raw, err := json.Marshal(msgs[0].Content[1])
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"type":"thinking","thinking":"hmm","signature":"sig"}` {
		t.Fatalf("thinking block = %s, want no cache_control", raw)
	}
}
