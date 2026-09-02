package evals

import (
	"fmt"
	"strings"
)

// Trajectory is the shape task 10.7 grades: not just whether a run ended in
// the right state, but HOW it got there — which tools it called, in what
// order, whether it asked instead of guessing when it should have, and how
// much it cost to get there. It is intentionally the same shape a real
// kernel run's event log could produce (every ToolCall name here is exactly
// a tool_use's ToolName, InputRequested is exactly whether an
// EventInputRequested was appended before the terminal event) — evals stays
// a leaf with respect to kernel/internal/store (this package doesn't import
// either), so a Trajectory here is filled in either by a corpus fixture
// (as this file's own tests and corpus_trajectory.go do, deterministically)
// or, once a harness exists to run a real session end-to-end against
// provider/fake under `go test`, by translating that session's own event
// log into this same shape — TrajectoryCase's Run signature doesn't change
// either way.
type Trajectory struct {
	ToolCalls      []string // tool names, in call order
	InputRequested bool     // whether the run raised an input_request instead of guessing
	Turns          int
}

// TrajectoryCase grades one recorded (or fixture) Trajectory against what
// was expected. Class is usually ClassCapability — this is "did the agent
// behave well," not "did the wire protocol survive," which is what
// provider_case.go's regression/negative cases already cover.
type TrajectoryCase struct {
	ID          string
	Class       Class
	Description string
	Expected    Trajectory
	Actual      Trajectory
	// ExpectInputRequest, when true, means the RIGHT behavior for this case
	// was to raise an input request rather than guess — task 10.7's
	// "whether an input request was raised instead of a guess."
	ExpectInputRequest bool
}

// GradeTrajectory checks tool-selection accuracy (the expected tool
// sequence, in order — an agent that calls the right tools in the wrong
// order took a different path, which is exactly the kind of thing
// end-state-only grading misses), the ask-vs-guess signal, and reports
// (never gates on, that's efficiency.go's job) turns/calls consumed.
func GradeTrajectory(c TrajectoryCase) Trial {
	if c.ExpectInputRequest != c.Actual.InputRequested {
		return Trial{
			CaseID:  c.ID,
			Verdict: VerdictFail,
			Detail: fmt.Sprintf(
				"input-request behavior: got raised=%v, want raised=%v — %s",
				c.Actual.InputRequested, c.ExpectInputRequest, askVsGuessDetail(c),
			),
		}
	}

	if !toolSequenceEqual(c.Expected.ToolCalls, c.Actual.ToolCalls) {
		return Trial{
			CaseID:  c.ID,
			Verdict: VerdictFail,
			Detail: fmt.Sprintf(
				"tool-selection accuracy: got [%s], want [%s]",
				strings.Join(c.Actual.ToolCalls, ","), strings.Join(c.Expected.ToolCalls, ","),
			),
		}
	}

	return Trial{
		CaseID:  c.ID,
		Verdict: VerdictPass,
		Detail: fmt.Sprintf(
			"tool sequence matched (%d calls), turns=%d, input_requested=%v",
			len(c.Actual.ToolCalls), c.Actual.Turns, c.Actual.InputRequested,
		),
	}
}

func askVsGuessDetail(c TrajectoryCase) string {
	if c.ExpectInputRequest {
		return "the case required raising an input request rather than guessing at missing information"
	}
	return "the case required proceeding on what was given, not stopping to ask"
}

func toolSequenceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// RunTrajectoryCases grades every case — a pure code grader (task 10.4):
// tool-selection accuracy and ask-vs-guess are both objectively checkable
// against a recorded trajectory, never subjective.
func RunTrajectoryCases(cases []TrajectoryCase) Report {
	var report Report
	for _, c := range cases {
		report.Trials = append(report.Trials, GradeTrajectory(c))
	}
	return report
}
