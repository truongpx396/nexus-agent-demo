package anthropic

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/truongpx396/nexus-agent/internal/provider"
)

func TestValidateRequest_AcceptsWhatTheAdapterSendsForAToolTurnWithThinking(t *testing.T) {
	tools := []provider.ToolSchema{
		{Name: "platform/shell@v1", Description: "run", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "mcp/github/list_prs@v1", Description: "list", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	names, err := provider.NewWireNames(tools)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(messagesRequest{
		Model: "m", MaxTokens: 100, System: toAnthSystem("sys"), Tools: toAnthTools(tools, names),
		Messages: toAnthMessages([]provider.Message{
			provider.TextMessage("user", "go"),
			{Role: "assistant", Blocks: []provider.ContentBlock{
				{Kind: provider.BlockThinking, Signature: "sig"}, // empty text, signature only
				{Kind: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "platform/shell@v1", Input: json.RawMessage(`{"cmd":"ls"}`)},
				{Kind: provider.BlockToolUse, ToolUseID: "tu2", ToolName: "mcp/github/list_prs@v1"}, // no input
			}},
			{Role: "tool", Blocks: []provider.ContentBlock{
				{Kind: provider.BlockToolResult, ToolUseID: "tu1", Text: "a b"},
				{Kind: provider.BlockToolResult, ToolUseID: "tu2", Text: "none"},
			}},
		}, names),
		Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRequest(body); err != nil {
		t.Fatalf("ValidateRequest rejected what the adapter sends: %v\n%s", err, body)
	}
}

func TestValidateRequest_RejectsEachDocumentedViolation(t *testing.T) {
	const okTool = `{"name":"platform_shell_v1","input_schema":{"type":"object"}}`
	user := func(text string) string {
		return `{"role":"user","content":[{"type":"text","text":"` + text + `"}]}`
	}
	assistantCall := func(blocks ...string) string {
		return `{"role":"assistant","content":[` + strings.Join(blocks, ",") + `]}`
	}
	call := func(id string) string {
		return `{"type":"tool_use","id":"` + id + `","name":"platform_shell_v1","input":{}}`
	}
	result := func(id string) string {
		return `{"type":"tool_result","tool_use_id":"` + id + `","content":"ok"}`
	}
	userResults := func(ids ...string) string {
		var parts []string
		for _, id := range ids {
			parts = append(parts, result(id))
		}
		return `{"role":"user","content":[` + strings.Join(parts, ",") + `]}`
	}
	req := func(tools string, msgs ...string) string {
		return `{"tools":[` + tools + `],"messages":[` + strings.Join(msgs, ",") + `]}`
	}

	valid := map[string]string{
		"a plain turn":                      req(okTool, user("hi")),
		"a call and its result":             req(okTool, user("hi"), assistantCall(call("a")), userResults("a")),
		"parallel calls, one result turn":   req(okTool, user("hi"), assistantCall(call("a"), call("b")), userResults("a", "b")),
		"results split over merged turns":   req(okTool, user("hi"), assistantCall(call("a"), call("b")), userResults("a"), userResults("b")),
		"a thinking block, signature only":  req(okTool, user("hi"), assistantCall(`{"type":"thinking","thinking":"","signature":"s"}`, call("a")), userResults("a")),
		"four cache breakpoints":            `{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"tools":[` + okTool + `],"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}},{"type":"text","text":"c","cache_control":{"type":"ephemeral"}}]}]}`,
		"a tool name at the 128 byte limit": req(`{"name":"`+strings.Repeat("a", 128)+`"}`, user("hi")),
	}
	for name, body := range valid {
		if err := ValidateRequest([]byte(body)); err != nil {
			t.Errorf("valid %q was rejected: %v", name, err)
		}
	}

	invalid := map[string]string{
		"not JSON":                         `{`,
		"no messages":                      req(okTool),
		"a tool name with a slash":         req(`{"name":"platform/shell@v1"}`, user("hi")),
		"a tool name with a dot":           req(`{"name":"platform_shell_v1.2"}`, user("hi")),
		"an empty tool name":               req(`{"name":""}`, user("hi")),
		"a tool name over 128 bytes":       req(`{"name":"`+strings.Repeat("a", 129)+`"}`, user("hi")),
		"the first message is assistant":   req(okTool, assistantCall(`{"type":"text","text":"hi"}`), user("hi")),
		"a replayed tool_use name":         req(okTool, user("hi"), assistantCall(`{"type":"tool_use","id":"a","name":"platform/shell@v1","input":{}}`), userResults("a")),
		"a tool_use with no input":         req(okTool, user("hi"), assistantCall(`{"type":"tool_use","id":"a","name":"platform_shell_v1"}`), userResults("a")),
		"a tool_use with a null input":     req(okTool, user("hi"), assistantCall(`{"type":"tool_use","id":"a","name":"platform_shell_v1","input":null}`), userResults("a")),
		"a tool_use with a string input":   req(okTool, user("hi"), assistantCall(`{"type":"tool_use","id":"a","name":"platform_shell_v1","input":"ls"}`), userResults("a")),
		"a thinking block, no thinking":    req(okTool, user("hi"), assistantCall(`{"type":"thinking","signature":"s"}`, call("a")), userResults("a")),
		"a thinking block, no signature":   req(okTool, user("hi"), assistantCall(`{"type":"thinking","thinking":"hmm"}`, call("a")), userResults("a")),
		"a call with no result":            req(okTool, user("hi"), assistantCall(call("a")), user("and then?")),
		"a request ending on a call":       req(okTool, user("hi"), assistantCall(call("a"))),
		"one of two calls unanswered":      req(okTool, user("hi"), assistantCall(call("a"), call("b")), userResults("a")),
		"a result for an unknown call":     req(okTool, user("hi"), assistantCall(call("a")), userResults("a", "ghost")),
		"a result answered twice":          req(okTool, user("hi"), assistantCall(call("a")), userResults("a"), userResults("a")),
		"a result with no call before":     req(okTool, user("hi"), userResults("a")),
		"an assistant turn over a pending": req(okTool, user("hi"), assistantCall(call("a")), `{"role":"user","content":[{"type":"text","text":"x"}]}`, assistantCall(call("b")), userResults("b")),
		"five cache breakpoints":           `{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"tools":[` + okTool + `],"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}},{"type":"text","text":"c","cache_control":{"type":"ephemeral"}},{"type":"text","text":"d","cache_control":{"type":"ephemeral"}}]}]}`,
	}
	for name, body := range invalid {
		err := ValidateRequest([]byte(body))
		if err == nil {
			t.Errorf("invalid %q was accepted", name)
			continue
		}
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("invalid %q: error %v does not wrap ErrInvalidRequest", name, err)
		}
	}
}

// What the adapter sent before R1 and R5 is exactly what the validator exists to catch.
func TestValidateRequest_CatchesTheRawNameAndTheThinkingBlockWithoutItsField(t *testing.T) {
	rawName := `{"tools":[{"name":"platform/shell@v1"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	if err := ValidateRequest([]byte(rawName)); err == nil || !strings.Contains(err.Error(), "platform/shell@v1") {
		t.Errorf("a raw internal reference as a tool name: %v, want it named in the error", err)
	}
	noThinking := `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"thinking","signature":"s"},{"type":"tool_use","id":"a","name":"t","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"ok"}]}]}`
	if err := ValidateRequest([]byte(noThinking)); err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Errorf("a thinking block with no thinking field: %v", err)
	}
}
