package evals

import "fmt"

// EfficiencyMetrics is what one run cost to reach its end state — the
// signal quality-only grading (provider_case.go, trajectory.go) never sees.
type EfficiencyMetrics struct {
	Tokens    int
	Turns     int
	ToolCalls int
}

// EfficiencyBand is the declared acceptable range for a case (task 10.8).
// Zero fields are treated as "no band declared for this dimension" (never
// checked), not as "must be zero" — a case usually bands the dimensions it
// actually cares about.
type EfficiencyBand struct {
	MaxTokens    int
	MaxTurns     int
	MaxToolCalls int
}

// Within reports whether m fits inside b.
func (b EfficiencyBand) Within(m EfficiencyMetrics) bool {
	if b.MaxTokens > 0 && m.Tokens > b.MaxTokens {
		return false
	}
	if b.MaxTurns > 0 && m.Turns > b.MaxTurns {
		return false
	}
	if b.MaxToolCalls > 0 && m.ToolCalls > b.MaxToolCalls {
		return false
	}
	return true
}

// EfficiencyCase pairs a declared band with a baseline and a candidate
// measurement (task 10.8: "a change holding its quality verdict while
// regressing tokens/turns/tool-calls past the declared band is blocked").
// Baseline is carried for the report/dashboard even though only Candidate
// is checked against Band — the point of 10.8 is the declared band is the
// gate, not "however much worse than last time," which would let a slow
// creep through one small regression at a time.
type EfficiencyCase struct {
	ID          string
	Class       Class
	Description string
	Band        EfficiencyBand
	Baseline    EfficiencyMetrics
	Candidate   EfficiencyMetrics
}

// GradeEfficiency BLOCKS (VerdictFail, not merely a reported number) when
// Candidate falls outside Band — this is what makes 10.8 a gate and not a
// dashboard tile.
func GradeEfficiency(c EfficiencyCase) Trial {
	if !c.Band.Within(c.Candidate) {
		return Trial{
			CaseID:  c.ID,
			Verdict: VerdictFail,
			Detail: fmt.Sprintf(
				"candidate {tokens=%d turns=%d tool_calls=%d} exceeds band {max_tokens=%d max_turns=%d max_tool_calls=%d} (baseline was {tokens=%d turns=%d tool_calls=%d})",
				c.Candidate.Tokens, c.Candidate.Turns, c.Candidate.ToolCalls,
				c.Band.MaxTokens, c.Band.MaxTurns, c.Band.MaxToolCalls,
				c.Baseline.Tokens, c.Baseline.Turns, c.Baseline.ToolCalls,
			),
		}
	}
	return Trial{
		CaseID:  c.ID,
		Verdict: VerdictPass,
		Detail: fmt.Sprintf(
			"candidate {tokens=%d turns=%d tool_calls=%d} within band",
			c.Candidate.Tokens, c.Candidate.Turns, c.Candidate.ToolCalls,
		),
	}
}

// RunEfficiencyCases grades every case.
func RunEfficiencyCases(cases []EfficiencyCase) Report {
	var report Report
	for _, c := range cases {
		report.Trials = append(report.Trials, GradeEfficiency(c))
	}
	return report
}
