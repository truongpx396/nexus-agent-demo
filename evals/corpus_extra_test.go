package evals

import (
	"context"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
)

func TestTrajectoryCorpusAllPass(t *testing.T) {
	report := RunTrajectoryCases(TrajectoryCorpus())
	if len(report.Trials) == 0 {
		t.Fatal("TrajectoryCorpus returned no cases")
	}
	for _, trial := range report.Trials {
		if trial.Verdict != VerdictPass {
			t.Errorf("case %s: %s (%s)", trial.CaseID, trial.Verdict, trial.Detail)
		}
	}
}

func TestEfficiencyCorpusAllPass(t *testing.T) {
	report := RunEfficiencyCases(EfficiencyCorpus())
	if len(report.Trials) == 0 {
		t.Fatal("EfficiencyCorpus returned no cases")
	}
	for _, trial := range report.Trials {
		if trial.Verdict != VerdictPass {
			t.Errorf("case %s: %s (%s)", trial.CaseID, trial.Verdict, trial.Detail)
		}
	}
}

// TestJudgeCorpusPassesOnceCalibrated wires a real Judge (backed by
// provider/fake, deterministic) through calibration and grading — the same
// path a live deployment's cross-family judge would take, minus the live
// model.
func TestJudgeCorpusPassesOnceCalibrated(t *testing.T) {
	j := &Judge{
		Provider: fake.New(
			scriptedContent("PASS: explains why and offers a path forward"),
			scriptedContent("FAIL: no explanation given"),
			scriptedContent("PASS: this refusal explains why and offers an alternative"),
		),
		AgreementFloor: 0.8,
	}
	if _, err := j.Calibrate(context.Background(), LabeledJudgeCalibrationSet()); err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if !j.Calibrated() {
		t.Fatal("judge did not calibrate above its floor against its own labeled set")
	}

	report := RunJudgeCases(context.Background(), j, JudgeCorpus())
	for _, trial := range report.Trials {
		if trial.Verdict != VerdictPass {
			t.Errorf("case %s: %s (%s)", trial.CaseID, trial.Verdict, trial.Detail)
		}
	}
}

func TestHeldOutCorpusLoadsAndPasses(t *testing.T) {
	heldOutFS, err := HeldOutCorpus()
	if err != nil {
		t.Fatalf("HeldOutCorpus: %v", err)
	}
	cases, err := LoadProviderScriptCases(heldOutFS)
	if err != nil {
		t.Fatalf("LoadProviderScriptCases(heldout): %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("held-out corpus loaded zero cases")
	}
	report := RunProviderScriptCases(cases)
	for _, trial := range report.Trials {
		if trial.Verdict != VerdictPass {
			t.Errorf("held-out case %s: %s (%s)", trial.CaseID, trial.Verdict, trial.Detail)
		}
	}
}

func TestHeldOutCorpusIsDisjointFromVisibleCorpus(t *testing.T) {
	visibleFS, err := Corpus()
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	visible, err := LoadProviderScriptCases(visibleFS)
	if err != nil {
		t.Fatalf("LoadProviderScriptCases(visible): %v", err)
	}
	heldOutFS, err := HeldOutCorpus()
	if err != nil {
		t.Fatalf("HeldOutCorpus: %v", err)
	}
	heldOut, err := LoadProviderScriptCases(heldOutFS)
	if err != nil {
		t.Fatalf("LoadProviderScriptCases(heldout): %v", err)
	}

	visibleIDs := map[string]bool{}
	for _, c := range visible {
		visibleIDs[c.ID] = true
	}
	for _, c := range heldOut {
		if visibleIDs[c.ID] {
			t.Errorf("held-out case %s shares an id with a visible case — it must be a distinct fixture, task 10.6", c.ID)
		}
	}
}
