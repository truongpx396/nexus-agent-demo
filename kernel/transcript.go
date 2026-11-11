package kernel

import "github.com/truongpx396/nexus-agent-demo/internal/provider"

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
