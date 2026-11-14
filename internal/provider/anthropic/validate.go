package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalidRequest is wrapped by every error ValidateRequest returns.
var ErrInvalidRequest = errors.New("anthropic: request would be rejected by the Messages API")

// toolNameRule is the Messages API's rule for a tool's name, in the tool list and in a
// tool_use block alike.
var toolNameRule = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// maxCacheBreakpoints is how many cache_control breakpoints one request may carry.
const maxCacheBreakpoints = 4

// ValidateRequest checks a request body, as Stream sends it, against the rules the
// Messages API documents and answers a violation with HTTP 400. It exists for tests:
// the scripted provider accepts internal tool names, the adapter tests build their
// prompts by hand, and the live evals use their own toy tools, so none of them ran the
// production path against the provider's rules, which is how a tool name the API refuses
// (`platform/shell@v1`) and a thinking block missing its `thinking` field went unseen.
// A test points the adapter's BaseURL at a server that runs this on every body.
//
// It enforces:
//   - every tool name, in the tool list and in a tool_use block, matches
//     ^[a-zA-Z0-9_-]{1,128}$;
//   - there is at least one message and the first has role "user";
//   - every tool_use in an assistant message is answered by a tool_result in the turn
//     that follows, and a tool_result answers a tool_use of the turn before it, once
//     (consecutive messages of one role are one turn: the API merges them);
//   - every thinking block has both a "thinking" and a "signature" field, even when the
//     thinking text is empty;
//   - every tool_use has an "input" that is a JSON object;
//   - at most four cache_control breakpoints in all.
//
// It reads structure only, so it has no opinion about content.
func ValidateRequest(body []byte) error {
	var req struct {
		System   []json.RawMessage `json:"system"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return invalid("the body is not a JSON request: %v", err)
	}

	breakpoints := 0
	for _, raw := range req.System {
		if hasField(raw, "cache_control") {
			breakpoints++
		}
	}
	for i, raw := range req.Tools {
		var tool struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil {
			return invalid("tools[%d] is not an object: %v", i, err)
		}
		if !toolNameRule.MatchString(tool.Name) {
			return invalid("tools[%d] is named %q, which does not match %s", i, tool.Name, toolNameRule)
		}
		if hasField(raw, "cache_control") {
			breakpoints++
		}
	}

	if len(req.Messages) == 0 {
		return invalid("there are no messages")
	}
	if req.Messages[0].Role != "user" {
		return invalid("the first message has role %q, want \"user\"", req.Messages[0].Role)
	}

	// pending holds the tool_use ids of the assistant turn just seen that have not
	// been answered yet; answered is every id answered so far.
	pending := map[string]bool{}
	answered := map[string]bool{}
	lastRole := ""
	for mi, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return invalid("messages[%d] has role %q, want \"user\" or \"assistant\"", mi, m.Role)
		}
		if m.Role == "assistant" && len(pending) > 0 && lastRole != "assistant" {
			return invalid("messages[%d] is an assistant turn but %d tool_use block(s) before it have no tool_result", mi, len(pending))
		}

		for bi, raw := range m.Content {
			var block struct {
				Type      string `json:"type"`
				Name      string `json:"name"`
				ID        string `json:"id"`
				ToolUseID string `json:"tool_use_id"`
			}
			if err := json.Unmarshal(raw, &block); err != nil {
				return invalid("messages[%d].content[%d] is not an object: %v", mi, bi, err)
			}
			if hasField(raw, "cache_control") {
				breakpoints++
			}
			where := fmt.Sprintf("messages[%d].content[%d]", mi, bi)
			switch block.Type {
			case "thinking":
				if !hasField(raw, "thinking") || !hasField(raw, "signature") {
					return invalid("%s is a thinking block without both a \"thinking\" and a \"signature\" field", where)
				}
			case "tool_use":
				if m.Role != "assistant" {
					return invalid("%s is a tool_use in a %s message", where, m.Role)
				}
				if !toolNameRule.MatchString(block.Name) {
					return invalid("%s is a tool_use named %q, which does not match %s", where, block.Name, toolNameRule)
				}
				if !fieldIsObject(raw, "input") {
					return invalid("%s is a tool_use with no \"input\" object", where)
				}
				if block.ID == "" {
					return invalid("%s is a tool_use with no id", where)
				}
				pending[block.ID] = true
			case "tool_result":
				if m.Role != "user" {
					return invalid("%s is a tool_result in a %s message", where, m.Role)
				}
				if answered[block.ToolUseID] {
					return invalid("%s is a second tool_result for %q", where, block.ToolUseID)
				}
				if !pending[block.ToolUseID] {
					return invalid("%s answers %q, which the assistant turn before it did not call", where, block.ToolUseID)
				}
				delete(pending, block.ToolUseID)
				answered[block.ToolUseID] = true
			}
		}
		lastRole = m.Role
	}
	if len(pending) > 0 {
		return invalid("the request ends with %d tool_use block(s) that have no tool_result", len(pending))
	}
	if breakpoints > maxCacheBreakpoints {
		return invalid("%d cache_control breakpoints, at most %d are allowed", breakpoints, maxCacheBreakpoints)
	}
	return nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, args...))
}

// hasField reports whether the JSON object raw has the named key, whatever its value.
func hasField(raw json.RawMessage, key string) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	_, ok := fields[key]
	return ok
}

// fieldIsObject reports whether the JSON object raw has key, holding a JSON object.
func fieldIsObject(raw json.RawMessage, key string) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	v, ok := fields[key]
	return ok && bytes.HasPrefix(bytes.TrimSpace(v), []byte("{"))
}
