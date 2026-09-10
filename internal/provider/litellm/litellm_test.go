package litellm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// canned is a realistic Chat Completions streaming body: a content delta
// split across two chunks, then an incrementally-streamed tool call, then
// finish_reason, then a separate terminal usage-only chunk, then "[DONE]" —
// fed through httptest.Server so this test proves the parsing
// deterministically, never calling a live model (constitution Principle IX).
const canned = "" +
	`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}` + "\n\n" +
	`data: {"choices":[{"delta":{"content":" world"},"finish_reason":null}]}` + "\n\n" +
	`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}` + "\n\n" +
	`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"nyc\"}"}}]},"finish_reason":null}]}` + "\n\n" +
	`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
	`data: {"choices":[],"usage":{"prompt_tokens":50,"completion_tokens":12,"prompt_tokens_details":{"cached_tokens":10}}}` + "\n\n" +
	`data: [DONE]` + "\n\n"

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
	var gotAuth, gotPath string
	srv := newTestServer(t, http.StatusOK, canned, func(r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		gotPath = r.URL.Path
	})
	defer srv.Close()

	p := &Provider{APIKey: "test-key", Model: "qwen2.5-local", BaseURL: srv.URL}
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
			// not produced by this adapter
		}
	}

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if text != "Hello world" {
		t.Fatalf("text = %q, want %q", text, "Hello world")
	}
	if toolUse.ToolUseID != "call_1" || toolUse.ToolName != "get_weather" || string(toolUse.Input) != `{"city":"nyc"}` {
		t.Fatalf("tool use chunk = %+v", toolUse)
	}
	if usage.InputUncached != 40 || usage.InputCacheRead != 10 || usage.OutputTokens != 12 {
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

func TestStreamLengthMapsToDoneMaxOutput(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4096}}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	srv := newTestServer(t, http.StatusOK, body, nil)
	defer srv.Close()

	p := &Provider{Model: "m", BaseURL: srv.URL}
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

func TestStreamNoFinishReasonBeforeDoneIsTruncated(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	srv := newTestServer(t, http.StatusOK, body, nil)
	defer srv.Close()

	p := &Provider{Model: "m", BaseURL: srv.URL}
	stream, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var lastErr error
	for {
		_, ok, err := stream.Next(context.Background())
		if err != nil {
			lastErr = err
			break
		}
		if !ok {
			break
		}
	}
	if lastErr == nil {
		t.Fatal("expected a truncation error, got none")
	}
}

func TestStreamRateLimitIsThrottleError(t *testing.T) {
	srv := newTestServer(t, http.StatusTooManyRequests,
		`{"error":{"type":"rate_limit_error","message":"rate limited"}}`, nil)
	defer srv.Close()

	p := &Provider{Model: "m", BaseURL: srv.URL}
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
		`{"error":{"type":"invalid_request_error","message":"This model's maximum context length is 4096 tokens"}}`, nil)
	defer srv.Close()

	p := &Provider{Model: "m", BaseURL: srv.URL}
	_, err := p.Stream(context.Background(), provider.Prompt{}, nil, provider.RunContext{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if provider.ClassifyTrigger(err) != provider.TriggerContextOverflow {
		t.Fatalf("ClassifyTrigger(%v) = %v, want context_overflow", err, provider.ClassifyTrigger(err))
	}
}

func TestToChatMessagesDropsThinkingAndSplitsToolResults(t *testing.T) {
	msgs := []provider.Message{
		{Role: "assistant", Blocks: []provider.ContentBlock{
			{Kind: provider.BlockThinking, Text: "reasoning..."},
			{Kind: provider.BlockText, Text: "checking weather"},
			{Kind: provider.BlockToolUse, ToolUseID: "call_1", ToolName: "get_weather", Input: []byte(`{"city":"nyc"}`)},
		}},
		{Role: "tool", Blocks: []provider.ContentBlock{
			{Kind: provider.BlockToolResult, ToolUseID: "call_1", Text: "sunny"},
		}},
	}
	out := toChatMessages("", msgs)
	if len(out) != 2 {
		t.Fatalf("got %d messages, want 2 (thinking block dropped): %+v", len(out), out)
	}
	if out[0].Role != "assistant" || out[0].Content != "checking weather" || len(out[0].ToolCalls) != 1 {
		t.Fatalf("assistant message = %+v", out[0])
	}
	if out[0].ToolCalls[0].Function.Arguments != `{"city":"nyc"}` {
		t.Fatalf("tool call args = %q", out[0].ToolCalls[0].Function.Arguments)
	}
	if out[1].Role != "tool" || out[1].Content != "sunny" || out[1].ToolCallID != "call_1" {
		t.Fatalf("tool result message = %+v", out[1])
	}
}
