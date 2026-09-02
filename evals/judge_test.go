package evals

import (
	"context"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
)

func scriptedContent(text string) fake.Script {
	return fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "content", Text: text},
		{Kind: "done", Done: "stop"},
	}}
}

func TestJudgeRefusesToGradeBeforeCalibration(t *testing.T) {
	j := &Judge{Provider: fake.New(scriptedContent("PASS looks fine")), AgreementFloor: 0.8}
	trial := j.Grade(context.Background(), JudgeCase{ID: "j1", Class: ClassCapability})
	if trial.Verdict != VerdictInconclusive {
		t.Fatalf("uncalibrated judge graded %s, want Inconclusive (task 10.5: never blocks before calibration)", trial.Verdict)
	}
}

func TestJudgeCalibratesAndGradesOnceAboveFloor(t *testing.T) {
	labeled := []JudgeCase{
		{ID: "l1", Prompt: "p1", Rubric: "r", WantVerdict: VerdictPass},
		{ID: "l2", Prompt: "p2", Rubric: "r", WantVerdict: VerdictFail},
	}
	// One script per Stream() call, in order: Calibrate consumes the first
	// two (matching the labels), Grade consumes the third.
	j := &Judge{
		Provider: fake.New(
			scriptedContent("PASS: matches the rubric"),
			scriptedContent("FAIL: does not match"),
			scriptedContent("PASS: the real grade"),
		),
		AgreementFloor: 0.8,
	}

	agreement, err := j.Calibrate(context.Background(), labeled)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if agreement != 1.0 {
		t.Fatalf("agreement = %v, want 1.0 (both labeled cases matched)", agreement)
	}
	if !j.Calibrated() {
		t.Fatal("Calibrated() = false after a perfect-agreement run above the floor")
	}

	trial := j.Grade(context.Background(), JudgeCase{ID: "real-case", Rubric: "r", Prompt: "p3"})
	if trial.Verdict != VerdictPass {
		t.Fatalf("Grade after calibration = %s, want Pass: %s", trial.Verdict, trial.Detail)
	}
}

func TestJudgeStaysUncalibratedBelowAgreementFloor(t *testing.T) {
	labeled := []JudgeCase{
		{ID: "l1", Prompt: "p1", Rubric: "r", WantVerdict: VerdictPass},
		{ID: "l2", Prompt: "p2", Rubric: "r", WantVerdict: VerdictFail},
	}
	j := &Judge{
		Provider: fake.New(
			scriptedContent("FAIL: disagrees with l1's PASS label"),
			scriptedContent("FAIL: agrees with l2"),
		),
		AgreementFloor: 0.9, // 1/2 = 0.5 agreement will not clear this
	}
	agreement, err := j.Calibrate(context.Background(), labeled)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if agreement != 0.5 {
		t.Fatalf("agreement = %v, want 0.5", agreement)
	}
	if j.Calibrated() {
		t.Fatal("Calibrated() = true despite falling below AgreementFloor")
	}
}

func TestRunJudgeCasesWithNilJudgeIsInconclusiveViaGate(t *testing.T) {
	g := &Gate{}
	result := g.Run(context.Background(), GateInput{
		JudgeCases: []JudgeCase{{ID: "j1", Class: ClassCapability, Rubric: "r", Prompt: "p"}},
	})
	if len(result.Cases) != 1 || result.Cases[0].Verdict != VerdictInconclusive {
		t.Fatalf("gate with no Judge configured must resolve a JudgeCase Inconclusive, got %+v", result.Cases)
	}
}
