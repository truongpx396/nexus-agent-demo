package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
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
	stream, err := p.Stream(context.Background(), provider.Prompt{System: "sys", Messages: []provider.Message{{Role: "user", Text: "hi"}}}, nil, provider.RunContext{})
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
