package evals

import "testing"

// TestEvalGate is the release gate itself: it must load and pass, or the
// build fails (Makefile's `eval` target runs the equivalent standalone
// binary; this test form is what `make test` and CI's `unit` job exercise).
func TestEvalGate(t *testing.T) {
	corpus, err := Corpus()
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	cases, err := LoadProviderScriptCases(corpus)
	if err != nil {
		t.Fatalf("LoadProviderScriptCases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("corpus loaded zero cases — the eval gate would pass vacuously")
	}

	report := RunProviderScriptCases(cases)
	for _, trial := range report.Trials {
		if trial.Verdict != VerdictPass {
			t.Errorf("case %s: %s (%s)", trial.CaseID, trial.Verdict, trial.Detail)
		}
	}
	if !report.Pass() {
		t.Fatal("eval gate did not pass")
	}
}

func TestEveryCaseDeclaresAKnownClass(t *testing.T) {
	corpus, err := Corpus()
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	cases, err := LoadProviderScriptCases(corpus)
	if err != nil {
		t.Fatalf("LoadProviderScriptCases: %v", err)
	}

	known := map[Class]bool{
		ClassRegression: true, ClassCapability: true, ClassSafety: true, ClassNegative: true,
	}
	for _, c := range cases {
		if !known[c.Class] {
			t.Errorf("case %s declares unknown class %q", c.ID, c.Class)
		}
		if c.ID == "" {
			t.Error("a case has no id")
		}
	}
}
