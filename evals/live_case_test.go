package evals

import (
	"context"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
)

// TestRunLiveTaskCase_ against the deterministic fake provider proves the
// harness itself — turn loop, tool-result pairing, Trajectory construction,
// grading — works correctly without needing a live model or an API key
// (README task 13.9's own code is otherwise only exercised under
// -tags=liveeval, which `go test ./...` never builds).
func TestRunLiveTaskCase_SingleToolCallPasses(t *testing.T) {
	prov := fake.New(
		fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "tool_use", ToolUseID: "tu1", ToolName: "get_weather", Input: `{"city":"Paris"}`},
			{Kind: "done", Done: "stop"},
		}},
		fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "content", Text: "It's cloudy and 14C in Paris."},
			{Kind: "done", Done: "stop"},
		}},
	)
	c := LiveTaskCase{
		ID:          "test-single-call",
		Class:       ClassCapability,
		Input:       "What's the weather in Paris?",
		Tools:       []provider.ToolSchema{weatherTool},
		ToolResults: map[string]LiveToolResult{"get_weather": {Text: `{"condition":"cloudy","temp_c":14}`}},
		Expected:    Trajectory{ToolCalls: []string{"get_weather"}},
	}
	trial := RunLiveTaskCase(context.Background(), prov, "test-model", nil, c)
	if trial.Verdict != VerdictPass {
		t.Fatalf("trial = %+v, want VerdictPass", trial)
	}
}

func TestRunLiveTaskCase_NoToolCallWhenNoneExpected(t *testing.T) {
	prov := fake.New(fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "content", Text: "2 + 2 is 4."},
		{Kind: "done", Done: "stop"},
	}})
	c := LiveTaskCase{
		ID:       "test-no-call",
		Class:    ClassCapability,
		Input:    "What is 2+2?",
		Expected: Trajectory{ToolCalls: nil},
	}
	trial := RunLiveTaskCase(context.Background(), prov, "test-model", nil, c)
	if trial.Verdict != VerdictPass {
		t.Fatalf("trial = %+v, want VerdictPass", trial)
	}
}

func TestRunLiveTaskCase_QuestionMarkCountsAsInputRequested(t *testing.T) {
	prov := fake.New(fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "content", Text: "Where would you like to fly to, and on what date?"},
		{Kind: "done", Done: "stop"},
	}})
	c := LiveTaskCase{
		ID:                 "test-clarify",
		Class:              ClassCapability,
		Input:              "Book me a flight.",
		Expected:           Trajectory{ToolCalls: nil},
		ExpectInputRequest: true,
	}
	trial := RunLiveTaskCase(context.Background(), prov, "test-model", nil, c)
	if trial.Verdict != VerdictPass {
		t.Fatalf("trial = %+v, want VerdictPass", trial)
	}
}

func TestRunLiveTaskCase_WrongToolSequenceFails(t *testing.T) {
	prov := fake.New(fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "tool_use", ToolUseID: "tu1", ToolName: "calculator", Input: `{}`},
		{Kind: "done", Done: "stop"},
	}}, fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "content", Text: "done"},
		{Kind: "done", Done: "stop"},
	}})
	c := LiveTaskCase{
		ID:          "test-wrong-tool",
		Class:       ClassCapability,
		Input:       "irrelevant",
		Tools:       []provider.ToolSchema{calculatorTool},
		ToolResults: map[string]LiveToolResult{"calculator": {Text: "42"}},
		Expected:    Trajectory{ToolCalls: []string{"get_weather"}},
	}
	trial := RunLiveTaskCase(context.Background(), prov, "test-model", nil, c)
	if trial.Verdict != VerdictFail {
		t.Fatalf("trial = %+v, want VerdictFail", trial)
	}
}

func TestRunLiveTaskCase_NilProviderIsInconclusive(t *testing.T) {
	trial := RunLiveTaskCase(context.Background(), nil, "test-model", nil, LiveTaskCase{ID: "test-nil"})
	if trial.Verdict != VerdictInconclusive {
		t.Fatalf("trial = %+v, want VerdictInconclusive", trial)
	}
}

func TestLiveTaskCorpus_NotEmptyAndEveryToolHasAResult(t *testing.T) {
	corpus := LiveTaskCorpus()
	if len(corpus) < 10 {
		t.Fatalf("LiveTaskCorpus has %d cases, want at least 10 (README task 13.9)", len(corpus))
	}
	seen := map[string]bool{}
	for _, c := range corpus {
		if seen[c.ID] {
			t.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
		for _, tool := range c.Tools {
			if _, ok := c.ToolResults[tool.Name]; !ok && len(c.ToolResults) > 0 {
				t.Errorf("case %s declares tool %q with no fixture result and a non-empty ToolResults map", c.ID, tool.Name)
			}
		}
	}
}
