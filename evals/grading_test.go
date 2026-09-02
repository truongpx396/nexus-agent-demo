package evals

import "testing"

func TestGradeTrajectoryToolSequenceMismatch(t *testing.T) {
	c := TrajectoryCase{
		ID:       "t1",
		Class:    ClassCapability,
		Expected: Trajectory{ToolCalls: []string{"file_read", "file_write"}},
		Actual:   Trajectory{ToolCalls: []string{"file_write", "file_read"}}, // right tools, wrong order
	}
	trial := GradeTrajectory(c)
	if trial.Verdict != VerdictFail {
		t.Fatalf("mismatched tool order = %s, want Fail: %s", trial.Verdict, trial.Detail)
	}
}

func TestGradeTrajectoryAskVsGuess(t *testing.T) {
	c := TrajectoryCase{
		ID:                 "t2",
		Class:              ClassCapability,
		ExpectInputRequest: true,
		Actual:             Trajectory{InputRequested: false}, // guessed instead of asking
	}
	trial := GradeTrajectory(c)
	if trial.Verdict != VerdictFail {
		t.Fatalf("guessed instead of asking = %s, want Fail: %s", trial.Verdict, trial.Detail)
	}
}

func TestGradeTrajectoryMatchPasses(t *testing.T) {
	c := TrajectoryCase{
		ID:       "t3",
		Class:    ClassCapability,
		Expected: Trajectory{ToolCalls: []string{"file_read"}, Turns: 2},
		Actual:   Trajectory{ToolCalls: []string{"file_read"}, Turns: 2},
	}
	trial := GradeTrajectory(c)
	if trial.Verdict != VerdictPass {
		t.Fatalf("matching trajectory = %s, want Pass: %s", trial.Verdict, trial.Detail)
	}
}

func TestGradeEfficiencyWithinBandPasses(t *testing.T) {
	c := EfficiencyCase{
		ID:        "e1",
		Class:     ClassCapability,
		Band:      EfficiencyBand{MaxTokens: 1000, MaxTurns: 5},
		Candidate: EfficiencyMetrics{Tokens: 800, Turns: 3},
	}
	trial := GradeEfficiency(c)
	if trial.Verdict != VerdictPass {
		t.Fatalf("within-band candidate = %s, want Pass: %s", trial.Verdict, trial.Detail)
	}
}

// TestGradeEfficiencyBlocksEvenWithoutABaselineComparison is task 10.8's
// own acceptance line made concrete: exceeding the declared band is a block
// regardless of whether it's worse than the baseline by any particular
// margin — the band, not the delta, is the gate.
func TestGradeEfficiencyBlocksEvenWithoutABaselineComparison(t *testing.T) {
	c := EfficiencyCase{
		ID:        "e2",
		Class:     ClassCapability,
		Band:      EfficiencyBand{MaxTokens: 1000},
		Baseline:  EfficiencyMetrics{Tokens: 900},
		Candidate: EfficiencyMetrics{Tokens: 1260}, // ~40% over baseline AND over band
	}
	trial := GradeEfficiency(c)
	if trial.Verdict != VerdictFail {
		t.Fatalf("over-band candidate = %s, want Fail: %s", trial.Verdict, trial.Detail)
	}
}

func TestHeldOutGapMeasuresWideningDivergence(t *testing.T) {
	visible := Report{Trials: []Trial{{Verdict: VerdictPass}, {Verdict: VerdictPass}, {Verdict: VerdictPass}, {Verdict: VerdictPass}}} // 100%
	heldOut := Report{Trials: []Trial{{Verdict: VerdictPass}, {Verdict: VerdictFail}}}                                                 // 50%

	gap := HeldOutGap(visible, heldOut)
	if gap <= 0 {
		t.Fatalf("HeldOutGap = %v, want > 0 when visible outperforms held-out", gap)
	}

	_, within := CheckHeldOutGap(visible, heldOut, HeldOutPolicy{MaxGap: 0.1})
	if within {
		t.Fatal("a 50-point gap should not be within a 0.1 policy")
	}
}

func TestHeldOutGapClampsNegativeToZero(t *testing.T) {
	visible := Report{Trials: []Trial{{Verdict: VerdictFail}}}
	heldOut := Report{Trials: []Trial{{Verdict: VerdictPass}}}
	if gap := HeldOutGap(visible, heldOut); gap != 0 {
		t.Fatalf("held-out outperforming visible should clamp to 0, got %v", gap)
	}
}
