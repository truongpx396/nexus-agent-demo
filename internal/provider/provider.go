// Package provider is the one abstraction every model call goes through
// (docs/constitution.md, Principle VII): native tool-calling only, no
// scattered SDK calls, usage split by token class so the cache-read gate is
// measurable rather than estimated.
package provider

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

// ChunkKind classifies a normalized stream chunk. The kernel (Phase 2)
// dispatches on this, never on parsing free-form text.
type ChunkKind string

const (
	ChunkContent   ChunkKind = "content"
	ChunkReasoning ChunkKind = "reasoning" // opaque, round-tripped, never shown
	ChunkToolUse   ChunkKind = "tool_use"
	ChunkUsage     ChunkKind = "usage"
	ChunkDone      ChunkKind = "done"
)

// DoneReason is carried on the terminal ChunkDone chunk of a stream.
type DoneReason string

const (
	DoneStop      DoneReason = "stop"
	DoneMaxOutput DoneReason = "max_output"
	DoneError     DoneReason = "error"
	// DoneRefusal is a policy decline (README task 13.7, closing F7) —
	// distinct from DoneStop so a safety refusal is never indistinguishable
	// from a successful empty completion, the exact failure mode
	// kernel/classify.go's typed classification exists to prevent one layer
	// up.
	DoneRefusal DoneReason = "refusal"
)

// Usage is split by token class — an undifferentiated total makes the
// >90% cache-read target unmeasurable (Principle III).
type Usage struct {
	InputUncached   int
	InputCacheRead  int
	InputCacheWrite int
	OutputTokens    int
}

// Chunk is one normalized unit of a provider's response stream.
type Chunk struct {
	Kind ChunkKind

	Text   string // ChunkContent
	Opaque []byte // ChunkReasoning — round-tripped, never shown to a caller

	ToolUseID string          // ChunkToolUse
	ToolName  string          // ChunkToolUse
	Input     json.RawMessage // ChunkToolUse

	Usage Usage // ChunkUsage

	Done            DoneReason // ChunkDone
	RefusalCategory string     // ChunkDone, only meaningful when Done == DoneRefusal (README task 13.7)
}

// ContentBlockKind classifies one block within a Message (README task 13.4,
// closing production-readiness finding F5): a Message widened from a flat
// Role/Text pair to typed content blocks so tool_use/tool_result pairing —
// and, on the same turn, a preceding thinking block — survives all the way
// to the wire, not just inside the kernel's own event log.
type ContentBlockKind string

const (
	BlockText       ContentBlockKind = "text"
	BlockThinking   ContentBlockKind = "thinking"
	BlockToolUse    ContentBlockKind = "tool_use"
	BlockToolResult ContentBlockKind = "tool_result"
)

// ContentBlock is one typed unit of a Message's content — which fields are
// meaningful depends on Kind (documented per field below), the same
// discriminated-union shape Chunk already uses for a provider's OUTPUT
// stream, now mirrored for the transcript sent as INPUT.
type ContentBlock struct {
	Kind ContentBlockKind

	Text      string // BlockText; BlockThinking (the visible reasoning text); BlockToolResult (the result text)
	Signature string // BlockThinking only — echoed back verbatim on the next turn, required when a thinking block precedes a tool_use in the same turn

	ToolUseID string          // BlockToolUse, BlockToolResult — pairs a result to the call that produced it
	ToolName  string          // BlockToolUse only
	Input     json.RawMessage // BlockToolUse only

	IsError bool // BlockToolResult only
}

// Message is one turn of the transcript sent to the provider — one role,
// one or more typed content blocks (an assistant turn may carry a thinking
// block, text, and one or more tool_use blocks together; a tool-result turn
// may carry several tool_result blocks together, one per call the prior
// turn made — never split across multiple messages, which silently trains
// a model to stop making parallel tool calls).
type Message struct {
	Role   string // "user" | "assistant" | "tool"
	Blocks []ContentBlock
}

// TextMessage builds a single-block plain-text Message — the common case
// (a user's input, a content-only assistant turn, a condensed summary) that
// doesn't need multiple blocks.
func TextMessage(role, text string) Message {
	return Message{Role: role, Blocks: []ContentBlock{{Kind: BlockText, Text: text}}}
}

// ToolResultMessage builds a single-block "tool" role Message pairing one
// result to the tool_use_id that produced it.
func ToolResultMessage(toolUseID, text string, isError bool) Message {
	return Message{Role: "tool", Blocks: []ContentBlock{{Kind: BlockToolResult, ToolUseID: toolUseID, Text: text, IsError: isError}}}
}

// PlainText concatenates every block's Text, newline-separated — what
// internal/promptctx's size/byte-stability logic and evals/judge.go's
// text-only judge prompts operate on; neither package cares about block
// structure, only the text a message carries.
func (m Message) PlainText() string {
	if len(m.Blocks) == 0 {
		return ""
	}
	var buf strings.Builder
	for i, b := range m.Blocks {
		if i > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(b.Text)
	}
	return buf.String()
}

// Prompt is the two-zone shape internal/promptctx builds (Phase 2): a
// stable system prompt plus the transcript so far. It intentionally has no
// per-turn free-form field — that discipline is enforced by promptctx, not
// by this type.
type Prompt struct {
	System   string
	Messages []Message
}

// ToolSchema is what the provider needs to expose tool-calling for one
// tool: identity, description, and JSON schema. internal/tools owns the
// richer Tool type this is projected from.
type ToolSchema struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// RunContext carries the identifiers a Provider call is attributed to.
type RunContext struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
}

// Stream is a normalized, provider-agnostic response stream. Next returns
// ok=false with a nil error at a clean end of stream (implementations MUST
// still have emitted a ChunkDone chunk before that point on any non-error
// path — provider/fake's Truncate flag exists specifically to test a caller
// that DOESN'T get one).
type Stream interface {
	Next(ctx context.Context) (chunk Chunk, ok bool, err error)
}

// Provider is the one abstraction all model access goes through. Native
// tool-calling only — no parsing tools out of free-form text.
type Provider interface {
	Stream(ctx context.Context, p Prompt, tools []ToolSchema, rc RunContext) (Stream, error)
}

// Embedding is one text's dense vector representation. A fixed width per
// Embedder implementation — internal/provider/fake's deterministic fake
// (README task 12.5) documents its own width as
// internal/retrieval.EmbeddingDimensions, and migrations/0022_retrieval.sql's
// `vector(32)` column is sized to match it exactly.
type Embedding []float32

// EmbedUsage is an Embed call's metered usage — one dimension, unlike
// Usage's four-way chat split, because an embedding call has no output
// tokens and no cache to measure (README task 12.4: "embedding calls ...
// are metered", not "metered identically to a chat call").
type EmbedUsage struct {
	Tokens int
}

// Embedder is the second model-call port this package exposes (README task
// 12.4, pattern #64/#67's own "reuses the Provider port" framing): embedding
// is a distinct capability from chat completion — no tool calling, no
// streaming, a different usage shape — so it gets its own narrow interface
// rather than an optional method bolted onto Provider. Every call is
// metered through internal/cost.Gate exactly like Provider.Stream
// (tests/contract's AST check, extended from task 4.8's original to cover
// this call site too); internal/provider/fake ships the only implementation
// this demo needs (task 12.5 — "no correctness test calls a live embedding
// model").
type Embedder interface {
	Embed(ctx context.Context, texts []string, rc RunContext) ([]Embedding, EmbedUsage, error)
}

// ThrottleError is returned by Stream itself (never via the Stream it would
// have returned) when the provider refuses the call outright — the
// Phase-2 failover taxonomy classifies this as retryable.
type ThrottleError struct {
	Reason string
}

func (e *ThrottleError) Error() string { return "provider throttled: " + e.Reason }

// ContextOverflowError is returned by Stream itself, or by a Stream's Next,
// when the provider refuses a call because the prompt exceeds the model's
// context window. internal/provider/failover.go classifies this as the one
// trigger that is never retried and never failed over to another provider —
// a smaller context window elsewhere doesn't fix an oversized prompt, and
// retrying the same provider with the same prompt can't either.
type ContextOverflowError struct {
	Reason string
}

func (e *ContextOverflowError) Error() string { return "provider context overflow: " + e.Reason }
