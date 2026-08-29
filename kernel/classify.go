package kernel

// Classification is the typed union a completed model turn is reduced to
// (README task 2.2). The loop dispatches on this value alone — never on
// inspecting text — so a turn's handling is a total switch over three cases
// instead of ad hoc string sniffing.
type Classification string

const (
	ClassificationToolCalls Classification = "TOOL_CALLS"
	ClassificationContent   Classification = "CONTENT"
	ClassificationEmpty     Classification = "EMPTY"
)

// Classify reduces one turn's assembled output to its Classification.
// Content and tool calls can arrive in the same turn; a tool call always
// takes dispatch precedence because the run isn't done until every tool_use
// has a paired result, regardless of any content alongside it.
func Classify(toolUses []ToolUseRequest, content string) Classification {
	switch {
	case len(toolUses) > 0:
		return ClassificationToolCalls
	case content != "":
		return ClassificationContent
	default:
		return ClassificationEmpty
	}
}
