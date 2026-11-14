package promptctx

import (
	"strings"
	"testing"

	"github.com/truongpx396/nexus-agent/internal/provider"
)

func TestEstimateTokens_EmptyIsZeroAndRoundsUp(t *testing.T) {
	if got := EstimateTokens("", nil, nil); got != 0 {
		t.Errorf("EstimateTokens of nothing = %d, want 0", got)
	}
	for n, want := range map[int]int64{1: 1, 2: 1, 3: 1, 4: 2, 6: 2, 7: 3} {
		if got := EstimateTokens(strings.Repeat("x", n), nil, nil); got != want {
			t.Errorf("EstimateTokens of %d bytes = %d, want %d (bytes divided by 3, rounded up)", n, got, want)
		}
	}
}

// Every part of what a call sends adds to the estimate.
func TestEstimateTokens_GrowsWithEachPartOfThePrompt(t *testing.T) {
	big := strings.Repeat("x", 3000) // 1,000 estimated tokens
	base := EstimateTokens("sys", nil, nil)

	tool := provider.ToolSchema{Name: "platform/echo@v1", Description: "echo", InputSchema: []byte(`{"type":"object"}`)}
	withTool := EstimateTokens("sys", []provider.ToolSchema{tool}, nil)
	if withTool <= base {
		t.Errorf("a tool did not raise the estimate: %d -> %d", base, withTool)
	}
	for name, schema := range map[string]provider.ToolSchema{
		"a long name":        {Name: big},
		"a long description": {Description: big},
		"a long schema":      {InputSchema: []byte(big)},
	} {
		if got := EstimateTokens("sys", []provider.ToolSchema{schema}, nil); got < base+1000 {
			t.Errorf("%s: estimate %d, want at least %d: each part of a tool is sent", name, got, base+1000)
		}
	}

	if got := EstimateTokens(big, nil, nil); got < 1000 {
		t.Errorf("a long system prompt: estimate %d, want at least 1000", got)
	}

	msgs := []provider.Message{
		provider.TextMessage("user", big),
		{Role: "assistant", Blocks: []provider.ContentBlock{
			{Kind: provider.BlockToolUse, ToolUseID: "tu", ToolName: "platform/echo@v1", Input: []byte(big)},
		}},
		provider.ToolResultMessage("tu", big, false),
	}
	if got := EstimateTokens("", nil, msgs[:1]); got < 1000 {
		t.Errorf("user text: estimate %d, want at least 1000", got)
	}
	if got := EstimateTokens("", nil, msgs[:2]); got < 2000 {
		t.Errorf("with a tool_use input: estimate %d, want at least 2000", got)
	}
	if got := EstimateTokens("", nil, msgs); got < 3000 {
		t.Errorf("with a tool_result: estimate %d, want at least 3000", got)
	}
}
