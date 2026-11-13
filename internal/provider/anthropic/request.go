package anthropic

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

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
//
// Its JSON tags (omitempty on every field) are used only to decode a request
// body back, as the tests do. What is sent is MarshalJSON's: each block type has
// fields the API requires even when empty (a thinking block's "thinking", a
// tool_use's "input") and fields it must not carry at all, which one struct with
// omitempty everywhere cannot express.
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

	// CacheControl is a cache breakpoint on this block (cacheLastBlock). The API
	// does not accept one on a thinking block, so MarshalJSON never emits it there.
	CacheControl *anthCacheControl `json:"cache_control,omitempty"`
}

// MarshalJSON emits exactly the fields the API defines for the block's Type:
//
//   - text: type, text.
//   - thinking: type, thinking and signature, both always. On the models this
//     adapter enables thinking for the thinking text is omitted by default, so a
//     block that comes back with only a signature must be sent back with an empty
//     "thinking" rather than none: the field is required.
//   - tool_use: type, id, name and input, which is {} when the model sent none.
//   - tool_result: type, tool_use_id, content (omitted when empty) and is_error
//     (omitted when false).
//   - cache_control, on any block but a thinking one, when a breakpoint was set.
//
// A Type this adapter does not produce is an error, not a silently empty block.
func (b anthContentBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case "text":
		return json.Marshal(struct {
			Type         string            `json:"type"`
			Text         string            `json:"text"`
			CacheControl *anthCacheControl `json:"cache_control,omitempty"`
		}{b.Type, b.Text, b.CacheControl})
	case "thinking":
		return json.Marshal(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{b.Type, b.Thinking, b.Signature})
	case "tool_use":
		input := b.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return json.Marshal(struct {
			Type         string            `json:"type"`
			ID           string            `json:"id"`
			Name         string            `json:"name"`
			Input        json.RawMessage   `json:"input"`
			CacheControl *anthCacheControl `json:"cache_control,omitempty"`
		}{b.Type, b.ID, b.Name, input, b.CacheControl})
	case "tool_result":
		return json.Marshal(struct {
			Type         string            `json:"type"`
			ToolUseID    string            `json:"tool_use_id"`
			Content      string            `json:"content,omitempty"`
			IsError      bool              `json:"is_error,omitempty"`
			CacheControl *anthCacheControl `json:"cache_control,omitempty"`
		}{b.Type, b.ToolUseID, b.Content, b.IsError, b.CacheControl})
	default:
		return nil, fmt.Errorf("anthropic: cannot serialise a %q content block", b.Type)
	}
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
//
// A replayed tool_use carries names.Wire(b.ToolName), never the internal
// reference: the API rejects a name outside [a-zA-Z0-9_-].
func toAnthMessages(msgs []provider.Message, names provider.WireNames) []anthMessage {
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
				blocks = append(blocks, anthContentBlock{Type: "tool_use", ID: b.ToolUseID, Name: names.Wire(b.ToolName), Input: b.Input})
			case provider.BlockToolResult:
				blocks = append(blocks, anthContentBlock{Type: "tool_result", ToolUseID: b.ToolUseID, Content: b.Text, IsError: b.IsError})
			}
		}
		out = append(out, anthMessage{Role: role, Content: blocks})
	}
	cacheLastBlock(out)
	return out
}

// cacheLastBlock puts an ephemeral cache breakpoint on the last block of the
// request that can carry one. In an agent loop the message history is most of the
// input and grows by a turn at a time, so a breakpoint at its end makes each call
// read the prefix the previous call wrote, where the system block's breakpoint
// alone covers only the tools and the system prompt (and nothing at all when those
// are shorter than the model's minimum cacheable length). A thinking block cannot
// carry one, so it is skipped. A request whose history is shorter than the
// minimum simply caches nothing: the API ignores a breakpoint it cannot use.
func cacheLastBlock(msgs []anthMessage) {
	for _, m := range slices.Backward(msgs) {
		for j := len(m.Content) - 1; j >= 0; j-- {
			if m.Content[j].Type == "thinking" {
				continue
			}
			m.Content[j].CacheControl = &anthCacheControl{Type: "ephemeral"}
			return
		}
	}
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

// toAnthTools lists the catalog under wire-safe names (provider.WireNames); the
// internal reference never reaches the API.
func toAnthTools(schemas []provider.ToolSchema, names provider.WireNames) []anthTool {
	if len(schemas) == 0 {
		return nil
	}
	out := make([]anthTool, 0, len(schemas))
	for _, s := range schemas {
		out = append(out, anthTool{Name: names.Wire(s.Name), Description: s.Description, InputSchema: s.InputSchema})
	}
	return out
}

// --- response / SSE event shapes ---
