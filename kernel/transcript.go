package kernel

import "github.com/truongpx396/nexus-agent/internal/provider"

// settleToolResult records a result as the model-facing answer to toolUseID.
//
// A suspended run's rehydrated transcript already holds an answer for the call
// it suspended on: the placeholder the suspension appended. Resuming replaces
// that block in place, so the prompt carries exactly one tool_result per
// tool_use. If there is none to replace, a new tool message is appended.
func (st *RunState) settleToolResult(toolUseID, text string, isError bool) {
	for i := range st.Transcript {
		m := &st.Transcript[i]
		if m.Role != "tool" {
			continue
		}
		for j := range m.Blocks {
			if b := &m.Blocks[j]; b.Kind == provider.BlockToolResult && b.ToolUseID == toolUseID {
				b.Text, b.IsError = text, isError
				return
			}
		}
	}
	st.Transcript = append(st.Transcript, provider.ToolResultMessage(toolUseID, text, isError))
}

// summaryPrefix opens the message that stands in for a condensed prefix.
const summaryPrefix = "[summary of the earlier conversation]\n"

// condensedTranscript is the transcript after a condensation: one message holding
// the summary, then every message from index covered on. A conversation has to
// open with a user turn, so the summary is a user message. The live loop and
// Rehydrate both build the condensed transcript here, so a session resumed from
// its log has the transcript the run that wrote the condensation had. It returns a
// new slice and leaves transcript alone.
func condensedTranscript(transcript []provider.Message, covered int, summary string) []provider.Message {
	out := make([]provider.Message, 0, 1+len(transcript)-covered)
	out = append(out, provider.TextMessage("user", summaryPrefix+summary))
	return append(out, transcript[covered:]...)
}
