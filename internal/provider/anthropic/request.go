package anthropic

import (
	"encoding/json"

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
