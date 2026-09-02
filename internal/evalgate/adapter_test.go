package evalgate

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/evals"
	"github.com/truongpx396/nexus-agent-demo/internal/plan"
)

func TestPlanGatePassesWhenTaggedCasesPass(t *testing.T) {
	planID := uuid.New()
	p := plan.Plan{PlanID: planID, Version: 3, Name: "triage-plan"}

	g := &evals.Gate{
		CaseArtifacts: map[string]evals.ArtifactRef{
			"plan-case-a": {Kind: evals.ArtifactPlan, ID: planID.String(), Version: 3},
		},
	}
	in := evals.GateInput{
		PermissionCases: []evals.PermissionScenarioCase{
			{ID: "plan-case-a", Class: evals.ClassSafety, Run: func() evals.Trial {
				return evals.Trial{CaseID: "plan-case-a", Verdict: evals.VerdictPass}
			}},
		},
	}

	gate := PlanGate(g, in)
	passed, detail, err := gate(context.Background(), p)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if !passed {
		t.Fatalf("expected pass, got fail: %s", detail)
	}
}

func TestPlanGateFailsClosedWithNoTaggedCases(t *testing.T) {
	p := plan.Plan{PlanID: uuid.New(), Version: 1, Name: "untested-plan"}
	g := &evals.Gate{} // nothing tagged for any artifact
	gate := PlanGate(g, evals.GateInput{PermissionCases: evals.AdversarialPermissionCorpus()})

	passed, _, err := gate(context.Background(), p)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if passed {
		t.Fatal("a plan version with no cases tagged to it must not pass vacuously")
	}
}

func TestPlanGateScopesToExactVersion(t *testing.T) {
	planID := uuid.New()
	g := &evals.Gate{
		CaseArtifacts: map[string]evals.ArtifactRef{
			"v1-only": {Kind: evals.ArtifactPlan, ID: planID.String(), Version: 1},
		},
	}
	in := evals.GateInput{
		PermissionCases: []evals.PermissionScenarioCase{
			{ID: "v1-only", Class: evals.ClassSafety, Run: func() evals.Trial {
				return evals.Trial{CaseID: "v1-only", Verdict: evals.VerdictPass}
			}},
		},
	}
	gate := PlanGate(g, in)

	// Version 2 of the SAME plan_id must not inherit version 1's cases.
	passed, _, err := gate(context.Background(), plan.Plan{PlanID: planID, Version: 2})
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if passed {
		t.Fatal("version 2 must not pass on version 1's tagged cases")
	}
}
