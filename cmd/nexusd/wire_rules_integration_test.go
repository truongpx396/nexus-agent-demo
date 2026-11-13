//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/inmem"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/anthropic"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// The review's findings R1, R2 and R5 were invisible because nothing ran the production
// path against the provider's own rules: the scripted provider accepts internal tool
// names, the adapter tests build their prompts by hand and the live evals use toy tools.
// This runs the real kernel, with the real resident catalog, through the real Anthropic
// adapter, against a server that applies anthropic.ValidateRequest to every request and
// answers with a scripted stream (a thinking block and two parallel tool calls), across a
// resume from the log. It fails on the first request the API would refuse.

// sse renders one scripted Messages API stream.
func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		kind := e[:strings.IndexByte(e, ' ')]
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", kind, e[strings.IndexByte(e, ' ')+1:])
	}
	return b.String()
}

func startEvent() string {
	return `message_start {"type":"message_start","message":{"usage":{"input_tokens":120}}}`
}

func endTurn(reason string) []string {
	return []string{
		`message_delta {"type":"message_delta","delta":{"stop_reason":"` + reason + `"},"usage":{"output_tokens":30}}`,
		`message_stop {"type":"message_stop"}`,
	}
}

// toolTurnStream is a model turn that thinks (the text omitted, only its signature, as the
// models this adapter enables thinking for do by default) and calls tools in parallel.
func toolTurnStream(sig string, calls ...[3]string) string {
	events := []string{
		startEvent(),
		`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + sig + `"}}`,
		`content_block_stop {"type":"content_block_stop","index":0}`,
	}
	for i, c := range calls { // id, wire name, input JSON ("" sends none)
		idx := i + 1
		events = append(events, fmt.Sprintf(`content_block_start {"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q}}`, idx, c[0], c[1]))
		if c[2] != "" {
			events = append(events, fmt.Sprintf(`content_block_delta {"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`, idx, c[2]))
		}
		events = append(events, fmt.Sprintf(`content_block_stop {"type":"content_block_stop","index":%d}`, idx))
	}
	return sse(append(events, endTurn("tool_use")...)...)
}

func textStream(text string) string {
	return sse(append([]string{
		startEvent(),
		`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		fmt.Sprintf(`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text),
		`content_block_stop {"type":"content_block_stop","index":0}`,
	}, endTurn("end_turn")...)...)
}

// validatingServer answers each request with the next scripted stream, after checking the
// body against the API's rules. A bad request is answered 400, as the API would, and noted.
type validatingServer struct {
	*httptest.Server
	mu       sync.Mutex
	bodies   [][]byte
	problems []string
}

func newValidatingServer(t *testing.T, script ...string) *validatingServer {
	t.Helper()
	v := &validatingServer{}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		v.mu.Lock()
		n := len(v.bodies)
		v.bodies = append(v.bodies, body)
		v.mu.Unlock()

		if err := anthropic.ValidateRequest(body); err != nil {
			v.mu.Lock()
			v.problems = append(v.problems, fmt.Sprintf("request %d: %v", n+1, err))
			v.mu.Unlock()
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"rejected by the test server"}}`))
			return
		}
		if n >= len(script) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(script[n]))
	}))
	t.Cleanup(v.Close)
	return v
}

func (v *validatingServer) requests() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.bodies)
}

// echoTools answers every call with a small result, so the run needs no sandbox or workspace.
type echoTools struct{}

func (echoTools) Execute(_ context.Context, req kernel.ToolUseRequest, _ kernel.ExecContext) kernel.ToolResult {
	return kernel.ToolResult{Output: json.RawMessage(`{"ran":"` + req.ToolName + `"}`)}
}

func wireRulesSeal(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
	return plaintext, []byte("digest"), "test-key", nil
}

func openPlain(_ context.Context, e eventlog.Event) ([]byte, error) { return e.Payload, nil }

// residentCatalog is the catalog nexusd shows the model, built by the same function
// serve() uses, with none of its dependencies reached: constructing the tools only
// stores them.
func residentCatalog(t *testing.T) []provider.ToolSchema {
	t.Helper()
	prev := devMode
	devMode = true // no NEXUS_SANDBOX needed to build the catalog
	t.Cleanup(func() { devMode = prev })
	stack, err := newToolPipeline(store.New(nil), nil, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("newToolPipeline: %v", err)
	}
	if len(stack.catalog) < 8 {
		t.Fatalf("the resident catalog has %d tools, want the full set", len(stack.catalog))
	}
	return stack.catalog
}

func TestAnthropicRequests_SatisfyTheProvidersRulesForTheRealCatalogAcrossAResume(t *testing.T) {
	catalog := residentCatalog(t)
	names := map[string]bool{}
	for _, c := range catalog {
		names[c.Name] = true
	}
	for _, want := range []string{"platform/shell@v1", "platform/file_read@v1", "platform/file_search@v1"} {
		if !names[want] {
			t.Fatalf("the resident catalog lacks %s", want)
		}
	}

	srv := newValidatingServer(t,
		// turn 1: thinking, then two calls in parallel
		toolTurnStream("sig-1", [3]string{"toolu_1", "platform_file_read_v1", `{"path":"a.txt"}`}, [3]string{"toolu_2", "platform_file_search_v1", ""}),
		textStream("I looked. What next?"),
		// after the resume: thinking again, then a shell call
		toolTurnStream("sig-2", [3]string{"toolu_3", "platform_shell_v1", `{"cmd":"ls"}`}),
		textStream("Done."),
	)

	log := inmem.New()
	k := &kernel.Kernel{
		Provider: provider.WithRetry(&anthropic.Provider{APIKey: "k", Model: "claude-sonnet-5", BaseURL: srv.URL, MaxTokens: 4096}, provider.RetryConfig{Attempts: 1}),
		Tools:    echoTools{},
		Budget:   kernel.NoopBudgetGate{},
		Log:      log,
	}
	tenantID, sessionID := uuid.New(), uuid.New()
	cfg := kernel.RunConfig{
		System: "You are a careful assistant.", Catalog: catalog, ModelID: "claude-sonnet-5", MaxTurns: 8,
		Input: "look around", AutonomyLevel: "autonomous", Conversational: true,
	}

	drain := func(label string, seq func(yield func(eventlog.Event, error) bool)) {
		t.Helper()
		for _, err := range seq {
			if err != nil {
				t.Fatalf("%s: %v\nrequests the server rejected: %v", label, err, srv.problems)
			}
		}
	}

	st := &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: wireRulesSeal}
	drain("first run", k.Run(context.Background(), st, cfg))
	if got := srv.requests(); got != 2 {
		t.Fatalf("the first run made %d requests, want 2 (a tool turn, then the reply)", got)
	}

	// The follow-up arrives out of band: every service that resumes rebuilds the state from the log.
	history := log.Events(sessionID)
	transcript, ids, err := kernel.Rehydrate(context.Background(), history, openPlain)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := kernel.RehydratePinned(context.Background(), history, openPlain)
	if err != nil {
		t.Fatal(err)
	}
	resumed := &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: wireRulesSeal, History: history, Transcript: transcript, ToolUseIDs: ids, Pinned: pinned}
	drain("resume", k.ResumeConversation(context.Background(), resumed, cfg, "now run ls"))

	if len(srv.problems) > 0 {
		t.Fatalf("the Messages API would have refused %d request(s):\n%s", len(srv.problems), strings.Join(srv.problems, "\n"))
	}
	if got := srv.requests(); got != 4 {
		t.Fatalf("%d requests in all, want 4", got)
	}

	// The names the model saw are wire-safe and the kernel still worked in internal references.
	first := string(srv.bodies[0])
	rule := regexp.MustCompile(`"name":"([^"]*)"`)
	for _, m := range rule.FindAllStringSubmatch(first, -1) {
		if strings.ContainsAny(m[1], "/@.") {
			t.Errorf("the first request sent the internal reference %q", m[1])
		}
	}
	var toolUses []string
	for _, e := range log.Events(sessionID) {
		if e.Type == eventlog.EventToolUse && e.ToolID != nil {
			toolUses = append(toolUses, *e.ToolID)
		}
	}
	if want := []string{"platform/file_read@v1", "platform/file_search@v1", "platform/shell@v1"}; strings.Join(toolUses, ",") != strings.Join(want, ",") {
		t.Errorf("the log records tool calls %v, want the internal references %v", toolUses, want)
	}
}
