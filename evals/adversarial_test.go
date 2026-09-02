package evals

import "testing"

// TestAdversarialPermissionCorpusAllPass proves every mandatory HITL
// adversarial case (README task 10.9) actually holds against the current
// internal/permissions.Chain — a corpus that only asserted properties
// nobody had verified would be worse than no corpus at all.
func TestAdversarialPermissionCorpusAllPass(t *testing.T) {
	report := RunPermissionScenarioCases(AdversarialPermissionCorpus())
	if len(report.Trials) == 0 {
		t.Fatal("AdversarialPermissionCorpus returned no cases")
	}
	for _, trial := range report.Trials {
		if trial.Verdict != VerdictPass {
			t.Errorf("case %s: %s (%s)", trial.CaseID, trial.Verdict, trial.Detail)
		}
	}
}

func TestAdversarialAdmissionCorpusAllPass(t *testing.T) {
	descriptorCases, skillCases := AdversarialAdmissionCorpus()

	descReport := RunDescriptorAdmissionCases(descriptorCases)
	for _, trial := range descReport.Trials {
		if trial.Verdict != VerdictPass {
			t.Errorf("descriptor case %s: %s (%s)", trial.CaseID, trial.Verdict, trial.Detail)
		}
	}

	skillReport := RunSkillCapabilityWideningCases(skillCases)
	for _, trial := range skillReport.Trials {
		if trial.Verdict != VerdictPass {
			t.Errorf("skill case %s: %s (%s)", trial.CaseID, trial.Verdict, trial.Detail)
		}
	}
}

// TestEveryAdversarialCaseDeclaresSafetyClass pins the whole point of task
// 10.9 — "mandatory" — to something a build actually checks: nobody can
// quietly reclassify one of these cases into a class that tolerates a
// failure.
func TestEveryAdversarialCaseDeclaresSafetyClass(t *testing.T) {
	for _, c := range AdversarialPermissionCorpus() {
		if c.Class != ClassSafety {
			t.Errorf("case %s declares class %q, want %q", c.ID, c.Class, ClassSafety)
		}
	}
	descriptorCases, skillCases := AdversarialAdmissionCorpus()
	for _, c := range descriptorCases {
		if c.Class != ClassSafety {
			t.Errorf("case %s declares class %q, want %q", c.ID, c.Class, ClassSafety)
		}
	}
	for _, c := range skillCases {
		if c.Class != ClassSafety {
			t.Errorf("case %s declares class %q, want %q", c.ID, c.Class, ClassSafety)
		}
	}
}
