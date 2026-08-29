// Package provider is the one abstraction every model call goes through
// (docs/constitution.md, Principle VII): native tool-calling only, no
// scattered SDK calls, usage split by token class so the cache-read gate is
// measurable rather than estimated.
package provider

import (
	"context"
	"encoding/json"

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

	Done DoneReason // ChunkDone
}

// Message is one turn of the transcript sent to the provider.
type Message struct {
	Role string // "user" | "assistant" | "tool"
	Text string
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
