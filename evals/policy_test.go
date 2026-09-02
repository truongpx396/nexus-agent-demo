package evals

import "testing"

func passTrials(n int) []Trial {
	trials := make([]Trial, n)
	for i := range trials {
		trials[i] = Trial{Verdict: VerdictPass}
	}
	return trials
}

func TestEvaluateCaseSafetyExactRequiredSingleFailureFails(t *testing.T) {
	trials := append(passTrials(9), Trial{Verdict: VerdictFail})
	verdict, _, passed, n := EvaluateCase(ClassSafety, nil, trials)
	if verdict != VerdictFail {
		t.Fatalf("safety case with 9/10 pass = %s, want Fail — task 10.1: no threshold below 100%%", verdict)
	}
	if passed != 9 || n != 10 {
		t.Fatalf("passed/n = %d/%d, want 9/10", passed, n)
	}
}

func TestEvaluateCaseSafetyAllPassPasses(t *testing.T) {
	verdict, _, _, _ := EvaluateCase(ClassSafety, nil, passTrials(5))
	if verdict != VerdictPass {
		t.Fatalf("safety case with 5/5 pass = %s, want Pass", verdict)
	}
}

func TestEvaluateCaseSafetyZeroTrialsIsInconclusiveNeverPass(t *testing.T) {
	verdict, _, _, _ := EvaluateCase(ClassSafety, nil, nil)
	if verdict != VerdictInconclusive {
		t.Fatalf("safety case with 0 trials = %s, want Inconclusive (never Pass, FR-138)", verdict)
	}
}

func TestEvaluateCaseCapabilityToleratesSomeSlack(t *testing.T) {
	// All-pass, but the Wilson lower bound needs a real sample size before
	// it clears a 0.9 threshold with 95% confidence — at n=20 it's still
	// only ~0.84 (see TestEvaluateCaseInconclusiveWhenIntervalStraddlesThreshold
	// for the n=1 extreme); n=50 comfortably clears it (~0.93).
	verdict, iv, _, _ := EvaluateCase(ClassCapability, nil, passTrials(50))
	if verdict != VerdictPass {
		t.Fatalf("50/50 capability trials = %s (interval %v), want Pass", verdict, iv)
	}
}

func TestEvaluateCaseCapabilityClearFailure(t *testing.T) {
	trials := make([]Trial, 20)
	for i := range trials {
		trials[i] = Trial{Verdict: VerdictFail}
	}
	verdict, iv, _, _ := EvaluateCase(ClassCapability, nil, trials)
	if verdict != VerdictFail {
		t.Fatalf("0/20 capability trials = %s (interval %v), want Fail", verdict, iv)
	}
}

func TestEvaluateCaseInconclusiveWhenIntervalStraddlesThreshold(t *testing.T) {
	// A single trial's interval is necessarily very wide — [0,1] worth of
	// uncertainty either side of a lone data point — so it should straddle
	// any mid-range threshold rather than confidently resolve either way.
	verdict, iv, _, _ := EvaluateCase(ClassCapability, nil, []Trial{{Verdict: VerdictPass}})
	if verdict != VerdictInconclusive {
		t.Fatalf("1/1 capability trial = %s (interval %v), want Inconclusive — one trial can't confidently clear a 0.9 threshold", verdict, iv)
	}
}

func TestPolicyForUnknownClassFailsClosed(t *testing.T) {
	p := policyFor(nil, Class("made_up_class"))
	if !p.ExactRequired || p.Threshold != 1.0 {
		t.Fatalf("policyFor(unknown class) = %+v, want the strictest fail-closed policy", p)
	}
}

func TestPolicyForOverride(t *testing.T) {
	custom := map[Class]ClassPolicy{ClassCapability: {Threshold: 0.5}}
	p := policyFor(custom, ClassCapability)
	if p.Threshold != 0.5 {
		t.Fatalf("policyFor with override = %+v, want Threshold 0.5", p)
	}
}
