package evals

import (
	"path/filepath"
	"testing"
)

func TestEnvironmentDigestSensitiveToEveryField(t *testing.T) {
	base := Environment{Image: "sandbox:v3", ResourceBand: "standard", Concurrency: 4, Region: "us"}
	mutations := map[string]func(*Environment){
		"Image":        func(e *Environment) { e.Image = "sandbox:v4" },
		"ResourceBand": func(e *Environment) { e.ResourceBand = "large" },
		"Concurrency":  func(e *Environment) { e.Concurrency = 8 },
		"Region":       func(e *Environment) { e.Region = "eu" },
	}
	baseDigest := base.DigestHex()
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			e := base
			mutate(&e)
			if e.DigestHex() == baseDigest {
				t.Fatalf("changing %s did not change the environment digest", name)
			}
		})
	}
}

func TestCompareEnvironmentsRefusesMismatch(t *testing.T) {
	a := Environment{Image: "sandbox:v3", Region: "us"}
	b := Environment{Image: "sandbox:v4", Region: "us"}
	if err := CompareEnvironments(a, a); err != nil {
		t.Fatalf("identical environments should compare cleanly, got %v", err)
	}
	if err := CompareEnvironments(a, b); err == nil {
		t.Fatal("mismatched environments should refuse comparison, got nil error")
	}
}

func TestCheckRegressionsRefusesAcrossEnvironments(t *testing.T) {
	baseline := Baseline{Environment: Environment{Image: "v1"}, Cases: map[string]CaseStat{}}
	candidate := GateResult{Environment: Environment{Image: "v2"}}
	if _, err := CheckRegressions(baseline, candidate); err == nil {
		t.Fatal("CheckRegressions should refuse to compare across environments")
	}
}

func TestCheckRegressionsDetectsIntervalSeparationNotAFlippedTrial(t *testing.T) {
	env := Environment{Image: "v1"}
	baseline := Baseline{
		Environment: env,
		Cases: map[string]CaseStat{
			"case-a": {N: 100, Pass: 100, Interval: WilsonInterval(100, 100, DefaultZ)},
			"case-b": {N: 20, Pass: 19, Interval: WilsonInterval(19, 20, DefaultZ)},
		},
	}
	candidate := GateResult{
		Environment: env,
		Cases: []CaseResult{
			// case-a regressed hard: 100/100 -> 50/100, intervals separate.
			{CaseID: "case-a", N: 100, Pass: 50, Interval: WilsonInterval(50, 100, DefaultZ)},
			// case-b lost exactly ONE trial out of 20 relative to baseline's
			// 19/20 — a "flipped trial," but the intervals still overlap,
			// so task 10.2 says this must NOT count as a regression.
			{CaseID: "case-b", N: 20, Pass: 18, Interval: WilsonInterval(18, 20, DefaultZ)},
			// case-c has no baseline entry: a new case is never a regression.
			{CaseID: "case-c", N: 5, Pass: 0, Interval: WilsonInterval(0, 5, DefaultZ)},
		},
	}

	regressions, err := CheckRegressions(baseline, candidate)
	if err != nil {
		t.Fatalf("CheckRegressions: %v", err)
	}
	if len(regressions) != 1 || regressions[0] != "case-a" {
		t.Fatalf("regressions = %v, want exactly [case-a]", regressions)
	}
}

func TestApplyRegressionsBlocksOverallPass(t *testing.T) {
	r := GateResult{OverallPass: true}
	r.ApplyRegressions([]string{"case-a"})
	if r.OverallPass {
		t.Fatal("ApplyRegressions with a non-empty list must flip OverallPass to false")
	}
	if len(r.Regressions) != 1 {
		t.Fatalf("Regressions = %v, want 1 entry", r.Regressions)
	}
}

func TestApplyRegressionsNoOpWhenClean(t *testing.T) {
	r := GateResult{OverallPass: true, Detail: "original detail"}
	r.ApplyRegressions(nil)
	if !r.OverallPass || r.Detail != "original detail" {
		t.Fatalf("ApplyRegressions(nil) must leave a passing result untouched, got %+v", r)
	}
}

func TestBaselineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")

	want := Baseline{
		Environment: Environment{Image: "sandbox:v3", Region: "us"},
		Cases: map[string]CaseStat{
			"case-a": {Class: ClassRegression, Pass: 5, N: 5, Interval: WilsonInterval(5, 5, DefaultZ), Verdict: VerdictPass},
		},
	}
	if err := SaveBaseline(path, want); err != nil {
		t.Fatalf("SaveBaseline: %v", err)
	}
	got, err := LoadBaseline(path)
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if got.Environment.DigestHex() != want.Environment.DigestHex() {
		t.Fatalf("round-tripped environment digest mismatch")
	}
	if len(got.Cases) != 1 || got.Cases["case-a"].Pass != 5 {
		t.Fatalf("round-tripped cases = %+v, want the original", got.Cases)
	}
}

func TestLoadBaselineMissingFileErrors(t *testing.T) {
	if _, err := LoadBaseline(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Fatal("LoadBaseline on a missing file should error, not return an empty Baseline")
	}
}
