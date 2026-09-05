package kernel

import "testing"

// TestClassify is README task 13.8 (closing production-readiness finding
// F9): kernel/ shipped with zero _test.go files despite being the loop
// itself. Table-driven over every precedence combination Classify's own
// doc comment claims: a tool call always wins, even alongside content.
func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		toolUses []ToolUseRequest
		content  string
		want     Classification
	}{
		{"no tool uses, no content", nil, "", ClassificationEmpty},
		{"no tool uses, content present", nil, "hello", ClassificationContent},
		{"tool use present, no content", []ToolUseRequest{{ToolName: "t"}}, "", ClassificationToolCalls},
		{"tool use present, content also present", []ToolUseRequest{{ToolName: "t"}}, "hello", ClassificationToolCalls},
		{"multiple tool uses", []ToolUseRequest{{ToolName: "a"}, {ToolName: "b"}}, "", ClassificationToolCalls},
		{"empty-slice tool uses (not nil) with no content", []ToolUseRequest{}, "", ClassificationEmpty},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.toolUses, c.content)
			if got != c.want {
				t.Errorf("Classify(%v, %q) = %q, want %q", c.toolUses, c.content, got, c.want)
			}
		})
	}
}
