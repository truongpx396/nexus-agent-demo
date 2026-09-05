package kernel

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// DecryptFunc reverses a SealFunc for one event: given the event whose
// KeyID/Payload/TenantID/SessionID identify what was sealed and how, it
// returns the plaintext. Its own seam, not internal/crypto directly, for
// the same reason SealFunc is: Rehydrate below stays free of any crypto/DB
// dependency.
type DecryptFunc func(ctx context.Context, e store.Event) (plaintext []byte, err error)

// reasoningOpaque is the shape a ChunkReasoning chunk's Opaque bytes decode
// as (README task 13.7): a provider adapter that supports extended thinking
// encodes {thinking, signature} into Opaque so the signature — required
// when a thinking block precedes a tool_use in the same turn — survives the
// round trip through the durable event log. A pre-13.7 or non-thinking
// provider's Opaque bytes simply fail this unmarshal, handled as "no
// thinking block to reconstruct," never an error.
type reasoningOpaque struct {
	Thinking  string `json:"thinking"`
	Signature string `json:"signature,omitempty"`
}

// Rehydrate replays a session's structural event history into the
// provider.Message transcript promptctx.Build works from — the SAME
// per-event-type grouping Run already builds up incrementally as it goes:
// an assistant turn's thinking/content/tool_use blocks are consolidated
// into ONE Message (README task 13.4), and every EventUserMessage/
// EventToolResult flushes it, exactly mirroring runTurns' own turn
// boundaries. Every other event type, including a bookkeeping one, is
// deliberately not part of the model-facing transcript, matching Run's own
// choices.
//
// The second return value maps each tool_use event's internal EventID to
// the provider-assigned tool_use_id it was reconstructed under — Resume/
// ResumeDelegation need this (via RunState.ToolUseIDs) to pair a resolved
// result back to the right wire-level block; it is derived here from
// already-durable state, never a value a caller could claim.
//
// This is deliberately narrow — pure structural replay plus one decrypt per
// event, no model call, no tool call, no append — scoped only to what
// Resume needs to rebuild a RunState's Transcript for the ONE
// approval/input-suspended case. It is not Phase 6's general replay (task
// 6.10's internal/runctl.Replay, which also rebuilds projections and
// verifies upcasting); that will generalize this the same way Phase 5's
// Resume itself is an honest interim for Phase 6's real Checkpoint-driven
// resume. history must already be in seq order (store.ListEvents' own
// contract).
func Rehydrate(ctx context.Context, history []store.Event, decrypt DecryptFunc) ([]provider.Message, map[uuid.UUID]string, error) {
	var transcript []provider.Message
	toolUseIDs := map[uuid.UUID]string{}

	var pendingAssistant []provider.ContentBlock
	var pendingResults []provider.ContentBlock
	flushAssistant := func() {
		if len(pendingAssistant) > 0 {
			transcript = append(transcript, provider.Message{Role: "assistant", Blocks: pendingAssistant})
			pendingAssistant = nil
		}
	}
	flushResults := func() {
		if len(pendingResults) > 0 {
			transcript = append(transcript, provider.Message{Role: "tool", Blocks: pendingResults})
			pendingResults = nil
		}
	}

	for _, e := range history {
		switch e.Type { //nolint:exhaustive // deliberately narrow: only the event types that ever became a transcript message during Run get one here too; every other type falls to default (see the comment below it)
		case store.EventThought:
			flushResults()
			var p thoughtPayload
			if err := decodeEvent(ctx, e, decrypt, &p); err != nil {
				return nil, nil, err
			}
			var r reasoningOpaque
			if err := json.Unmarshal(p.Opaque, &r); err == nil && (r.Thinking != "" || r.Signature != "") {
				pendingAssistant = append(pendingAssistant, provider.ContentBlock{Kind: provider.BlockThinking, Text: r.Thinking, Signature: r.Signature})
			}
		case store.EventContent:
			flushResults()
			var p contentPayload
			if err := decodeEvent(ctx, e, decrypt, &p); err != nil {
				return nil, nil, err
			}
			pendingAssistant = append(pendingAssistant, provider.ContentBlock{Kind: provider.BlockText, Text: p.Body})
		case store.EventToolUse:
			flushResults()
			var p toolUsePayload
			if err := decodeEvent(ctx, e, decrypt, &p); err != nil {
				return nil, nil, err
			}
			toolUseIDs[e.EventID] = p.ToolUseID
			pendingAssistant = append(pendingAssistant, provider.ContentBlock{Kind: provider.BlockToolUse, ToolUseID: p.ToolUseID, ToolName: p.ToolName, Input: p.Input})
		case store.EventUserMessage:
			flushAssistant()
			flushResults()
			var p userMessagePayload
			if err := decodeEvent(ctx, e, decrypt, &p); err != nil {
				return nil, nil, err
			}
			transcript = append(transcript, provider.TextMessage("user", p.Body))
		case store.EventToolResult:
			flushAssistant()
			var p toolResultPayload
			if err := decodeEvent(ctx, e, decrypt, &p); err != nil {
				return nil, nil, err
			}
			var toolUseID string
			if e.PairRef != nil {
				toolUseID = toolUseIDs[*e.PairRef]
			}
			pendingResults = append(pendingResults, provider.ContentBlock{Kind: provider.BlockToolResult, ToolUseID: toolUseID, Text: resultText(ToolResult(p)), IsError: p.IsError})
		default:
			// Everything else (tool_loaded, budget_decision, approval_*,
			// input_*, terminal, erasure, ...) is audit/control-plane
			// bookkeeping, not a conversation turn — Run itself never adds
			// any of these to Transcript either.
		}
	}
	flushAssistant()
	flushResults()
	return transcript, toolUseIDs, nil
}

func decodeEvent(ctx context.Context, e store.Event, decrypt DecryptFunc, out any) error {
	plaintext, err := decrypt(ctx, e)
	if err != nil {
		return fmt.Errorf("kernel: decrypt event %s (%s): %w", e.EventID, e.Type, err)
	}
	if err := json.Unmarshal(plaintext, out); err != nil {
		return fmt.Errorf("kernel: unmarshal %s payload for event %s: %w", e.Type, e.EventID, err)
	}
	return nil
}
