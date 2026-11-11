package provider

import (
	"errors"
	"fmt"
)

// ErrMalformedTranscript is wrapped by ValidateTranscript. A hosted model API
// answers a transcript like this with a 400, so a Provider that checks first
// turns a latent bug into a failure a test can see.
var ErrMalformedTranscript = errors.New("malformed transcript")

// ValidateTranscript checks the tool_use / tool_result pairing every hosted
// model API requires of the messages it is sent, and that the kernel
// guarantees (README Principle II, the paired-result invariant):
//
//   - a tool_use id is unique across the transcript;
//   - every tool_use is answered by exactly one tool_result;
//   - a tool_result answers a tool_use from an earlier assistant message, once;
//   - the answers come before the next assistant message, and nothing but
//     tool results sits between a tool_use and its answers.
//
// It reads structure only, so it is cheap and has no opinion about content.
// Consecutive tool messages are fine: a hosted API merges them into one turn.
func ValidateTranscript(messages []Message) error {
	seen := map[string]bool{}    // every tool_use id so far
	pending := map[string]bool{} // tool_use ids still waiting for their result

	for i, m := range messages {
		switch m.Role {
		case "assistant":
			if len(pending) > 0 {
				return fmt.Errorf("%w: message %d is an assistant turn but %d tool_use block(s) from before it have no tool_result", ErrMalformedTranscript, i, len(pending))
			}
			for _, b := range m.Blocks {
				if b.Kind != BlockToolUse {
					continue
				}
				if seen[b.ToolUseID] {
					return fmt.Errorf("%w: message %d repeats tool_use id %q", ErrMalformedTranscript, i, b.ToolUseID)
				}
				seen[b.ToolUseID] = true
				pending[b.ToolUseID] = true
			}
		case "tool":
			for _, b := range m.Blocks {
				if b.Kind != BlockToolResult {
					continue
				}
				if !seen[b.ToolUseID] {
					return fmt.Errorf("%w: message %d has a tool_result for %q, which no earlier tool_use has", ErrMalformedTranscript, i, b.ToolUseID)
				}
				if !pending[b.ToolUseID] {
					return fmt.Errorf("%w: message %d has a second tool_result for tool_use %q", ErrMalformedTranscript, i, b.ToolUseID)
				}
				delete(pending, b.ToolUseID)
			}
		default: // "user"
			if len(pending) > 0 {
				return fmt.Errorf("%w: message %d is a user turn between a tool_use and its tool_result", ErrMalformedTranscript, i)
			}
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("%w: %d tool_use block(s) at the end have no tool_result", ErrMalformedTranscript, len(pending))
	}
	return nil
}
