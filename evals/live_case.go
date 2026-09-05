package evals

import (
	"context"
	"fmt"
	"strings"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// LiveToolResult is the canned response one LiveTaskCase's tool returns when
// a LIVE model calls it — deterministic on purpose: this harness grades
// what the model chooses to call and in what order (GradeTrajectory,
// trajectory.go), not the tool's own execution, which stays a fixture the
// same way ProviderScriptCase's scripted chunks are a fixture for the fake
// provider's output.
type LiveToolResult struct {
	Text    string
	IsError bool
}

// LiveTaskCase is one real task-completion case (README task 13.9, closing
// production-readiness finding F6: "the release gate never calls a model").
// Unlike ProviderScriptCase (scripts what the FAKE provider emits on a
// canned stream), this drives an ACTUAL provider.Provider through a small
// tool-calling loop and grades the resulting Trajectory with the same code
// grader ordinary fixture-based TrajectoryCases already use —
// trajectory.go's own doc comment anticipated exactly this: "once a harness
// exists to run a real session end-to-end ... by translating that
// session's own event log into this same shape." This is that harness,
// minus the kernel/store machinery evals deliberately never imports (evals
// stays a leaf with respect to both).
//
// cmd/runner's `-tags=liveeval` file is the only thing that ever
// constructs a real provider.Provider and actually calls RunLiveTaskCase
// with one — this type and RunLiveTaskCase compile and are unit-testable
// under the default build too (against a fake provider), so a mistake here
// is caught by `go test ./evals/...`, not only by a build tagged out of
// every normal run.
type LiveTaskCase struct {
	ID          string
	Class       Class
	Description string
	System      string
	Input       string
	Tools       []provider.ToolSchema
	// ToolResults maps a tool name (provider.ToolSchema.Name) to the canned
	// result this harness returns when the model calls it. A tool the model
	// calls with no entry here gets a synthetic error result — same
	// fail-closed default kernel.Hygiene's own synthetic results use for an
	// unresolvable call.
	ToolResults map[string]LiveToolResult
	Expected    Trajectory
	// ExpectInputRequest mirrors TrajectoryCase's own field: the case's
	// PROMPT is deliberately under-specified, and the right behavior is to
	// ask rather than guess. This harness approximates "asked" as "the
	// model's final content-only turn contains a question mark" — a
	// deliberately simple, code-grader-appropriate proxy (no second judge
	// call) good enough for a small, hand-authored corpus; a real
	// input_request tool is what kernel-level cases use instead once one
	// exists in the resident catalog.
	ExpectInputRequest bool
	// MaxTurns bounds the loop (default 6) — a case that hasn't converged
	// by then is a fail, not a hang.
	MaxTurns int
}

func (c LiveTaskCase) maxTurns() int {
	if c.MaxTurns > 0 {
		return c.MaxTurns
	}
	return 6
}

// RunLiveTaskCase drives prov through c's task turn by turn, grades the
// resulting Trajectory with GradeTrajectory, and returns the Trial. Every
// call is metered exactly like Judge.gradeOnce (README task 4.8: "off the
// paying loop" still means a Purpose-tagged Reserve, never unmetered) — a
// nil budget defaults to noopBudgetGate, the same honest-default judge.go
// already establishes for this package.
func RunLiveTaskCase(ctx context.Context, prov provider.Provider, modelID string, budget BudgetGate, c LiveTaskCase) Trial {
	if prov == nil {
		return Trial{CaseID: c.ID, Verdict: VerdictInconclusive, Detail: "no live Provider configured"}
	}
	if budget == nil {
		budget = noopBudgetGate{}
	}

	messages := []provider.Message{provider.TextMessage("user", c.Input)}
	var toolCalls []string
	inputRequested := false
	turn := 0

	for ; turn < c.maxTurns(); turn++ {
		if _, err := budget.Reserve(ctx, cost.ReserveRequest{ModelID: modelID, Purpose: cost.PurposeTurn}); err != nil {
			return Trial{CaseID: c.ID, Verdict: VerdictInconclusive, Detail: fmt.Sprintf("reserve: %v", err)}
		}

		stream, err := prov.Stream(ctx, provider.Prompt{System: c.System, Messages: messages}, c.Tools, provider.RunContext{})
		if err != nil {
			return Trial{CaseID: c.ID, Verdict: VerdictInconclusive, Detail: fmt.Sprintf("stream: %v", err)}
		}

		var contentText strings.Builder
		var toolUses []provider.Chunk
		for {
			chunk, ok, nerr := stream.Next(ctx)
			if nerr != nil {
				return Trial{CaseID: c.ID, Verdict: VerdictInconclusive, Detail: fmt.Sprintf("stream.Next: %v", nerr)}
			}
			if !ok {
				break
			}
			switch chunk.Kind { //nolint:exhaustive // this harness only needs content/tool_use to build a Trajectory; reasoning/usage/done carry nothing GradeTrajectory reads
			case provider.ChunkContent:
				contentText.WriteString(chunk.Text)
			case provider.ChunkToolUse:
				toolUses = append(toolUses, chunk)
			}
		}

		if len(toolUses) == 0 {
			inputRequested = strings.Contains(contentText.String(), "?")
			turn++
			break
		}

		var assistantBlocks []provider.ContentBlock
		if contentText.Len() > 0 {
			assistantBlocks = append(assistantBlocks, provider.ContentBlock{Kind: provider.BlockText, Text: contentText.String()})
		}
		var resultBlocks []provider.ContentBlock
		for _, tu := range toolUses {
			toolCalls = append(toolCalls, tu.ToolName)
			assistantBlocks = append(assistantBlocks, provider.ContentBlock{
				Kind: provider.BlockToolUse, ToolUseID: tu.ToolUseID, ToolName: tu.ToolName, Input: tu.Input,
			})
			result, ok := c.ToolResults[tu.ToolName]
			if !ok {
				result = LiveToolResult{Text: fmt.Sprintf("error: no fixture result for tool %q", tu.ToolName), IsError: true}
			}
			resultBlocks = append(resultBlocks, provider.ContentBlock{
				Kind: provider.BlockToolResult, ToolUseID: tu.ToolUseID, Text: result.Text, IsError: result.IsError,
			})
		}
		messages = append(messages,
			provider.Message{Role: "assistant", Blocks: assistantBlocks},
			provider.Message{Role: "tool", Blocks: resultBlocks},
		)
	}

	actual := Trajectory{ToolCalls: toolCalls, InputRequested: inputRequested, Turns: turn}
	return GradeTrajectory(TrajectoryCase{
		ID: c.ID, Class: c.Class, Expected: c.Expected, Actual: actual, ExpectInputRequest: c.ExpectInputRequest,
	})
}

// RunLiveTaskCases grades every case with prov, in order.
func RunLiveTaskCases(ctx context.Context, prov provider.Provider, modelID string, budget BudgetGate, cases []LiveTaskCase) Report {
	var report Report
	for _, c := range cases {
		report.Trials = append(report.Trials, RunLiveTaskCase(ctx, prov, modelID, budget, c))
	}
	return report
}
