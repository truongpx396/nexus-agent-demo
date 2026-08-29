package kernel

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// DecryptFunc reverses a SealFunc for one event: given the event whose
// KeyID/Payload/TenantID/SessionID identify what was sealed and how, it
// returns the plaintext. Its own seam, not internal/crypto directly, for
// the same reason SealFunc is: Rehydrate below stays free of any crypto/DB
// dependency.
type DecryptFunc func(ctx context.Context, e store.Event) (plaintext []byte, err error)

// Rehydrate replays a session's structural event history into the
// provider.Message transcript promptctx.Build works from — the SAME
// per-event-type formatting Run already builds up incrementally as it goes
// (userMessagePayload -> "user", contentPayload -> "assistant",
// toolUsePayload -> "assistant", toolResultPayload -> "tool"; every other
// event type, including thought, is deliberately not part of the
// model-facing transcript, matching Run's own choices), factored out here
// so Resume (README task 5.8) doesn't duplicate it.
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
func Rehydrate(ctx context.Context, history []store.Event, decrypt DecryptFunc) ([]provider.Message, error) {
	var transcript []provider.Message
	for _, e := range history {
		switch e.Type { //nolint:exhaustive // deliberately narrow: only the 4 event types that ever became a transcript message during Run get one here too; every other type falls to default (see the comment below it)
		case store.EventUserMessage:
			var p userMessagePayload
			if err := decodeEvent(ctx, e, decrypt, &p); err != nil {
				return nil, err
			}
			transcript = append(transcript, provider.Message{Role: "user", Text: p.Body})
		case store.EventContent:
			var p contentPayload
			if err := decodeEvent(ctx, e, decrypt, &p); err != nil {
				return nil, err
			}
			transcript = append(transcript, provider.Message{Role: "assistant", Text: p.Body})
		case store.EventToolUse:
			var p toolUsePayload
			if err := decodeEvent(ctx, e, decrypt, &p); err != nil {
				return nil, err
			}
			transcript = append(transcript, provider.Message{
				Role: "assistant",
				Text: fmt.Sprintf("[tool_use %s] %s(%s)", e.EventID, p.ToolName, string(p.Input)),
			})
		case store.EventToolResult:
			var p toolResultPayload
			if err := decodeEvent(ctx, e, decrypt, &p); err != nil {
				return nil, err
			}
			transcript = append(transcript, provider.Message{Role: "tool", Text: resultText(ToolResult(p))})
		default:
			// Everything else (thought, tool_loaded, budget_decision,
			// approval_*, input_*, terminal, erasure, ...) is audit/
			// control-plane bookkeeping, not a conversation turn — Run
			// itself never adds any of these to Transcript either.
		}
	}
	return transcript, nil
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
