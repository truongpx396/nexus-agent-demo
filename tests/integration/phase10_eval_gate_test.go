//go:build integration

// Phase 10 — Eval gate hardening + go-live (README.md §10). Proves the one
// concrete promotion-gate wiring named in task 10.10: internal/evalgate.
// PlanGate, built from a real evals.Gate, driving internal/plan.Lifecycle.
// RunEvalGate end to end against a real Postgres-backed orchestration_plans
// row — the seam plan.EvalGate's own doc comment names ("cmd/nexusd wires
// the real evals.Report-backed gate in") but that, before this file, had
// no test exercising it at all (Phase 8 shipped the hook; nothing ever
// called it). Shares setupOversightRig with phase5_oversight_test.go (same
// package).
package integration

import (
	"context"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/evals"
	"github.com/truongpx396/nexus-agent-demo/internal/evalgate"
	"github.com/truongpx396/nexus-agent-demo/internal/plan"
)

func minimalPlan(name string) plan.Plan {
	return plan.Plan{
		Name: name, StartStep: "end",
		Steps: []plan.Step{{ID: "end", Kind: plan.StepCondition, Condition: &plan.ConditionConfig{}}},
	}
}

// TestPlanEvalGate_PassesWithTaggedCasesAndAdvancesLifecycle is the happy
// path: a plan version with a passing case tagged to its own
// (plan_id, version) clears RunEvalGate and the row advances
// validated -> eval_passed, exactly as internal/plan/lifecycle.go promises.
func TestPlanEvalGate_PassesWithTaggedCasesAndAdvancesLifecycle(t *testing.T) {
	r := setupOversightRig(t)
	ctx := context.Background()
	lc := &plan.Lifecycle{Store: r.st}

	created, err := lc.Create(ctx, r.tenantID, minimalPlan("eval-gate-happy-path"), "test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := lc.ValidatePlan(ctx, r.tenantID, created.PlanID, created.Version); err != nil {
		t.Fatalf("ValidatePlan: %v", err)
	}

	caseID := "plan-case-" + created.PlanID.String()
	g := &evals.Gate{
		CaseArtifacts: map[string]evals.ArtifactRef{
			caseID: {Kind: evals.ArtifactPlan, ID: created.PlanID.String(), Version: created.Version},
		},
	}
	in := evals.GateInput{
		PermissionCases: []evals.PermissionScenarioCase{
			{ID: caseID, Class: evals.ClassSafety, Run: func() evals.Trial {
				return evals.Trial{CaseID: caseID, Verdict: evals.VerdictPass}
			}},
		},
	}

	passed, detail, err := lc.RunEvalGate(ctx, r.tenantID, created.PlanID, created.Version, evalgate.PlanGate(g, in))
	if err != nil {
		t.Fatalf("RunEvalGate: %v", err)
	}
	if !passed {
		t.Fatalf("expected the gate to pass: %s", detail)
	}

	_, status, err := lc.Get(ctx, r.tenantID, created.PlanID, created.Version)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if status != plan.StatusEvalPassed {
		t.Fatalf("plan status = %s, want %s", status, plan.StatusEvalPassed)
	}
}

// TestPlanEvalGate_NoTaggedCasesFailsClosedAndLeavesPlanValidated proves the
// fail-closed default: a plan version nothing has ever gated stays refused
// (never a vacuous pass) and the row stays at validated — fixable and
// re-runnable, per RunEvalGate's own doc comment, never a dead end.
func TestPlanEvalGate_NoTaggedCasesFailsClosedAndLeavesPlanValidated(t *testing.T) {
	r := setupOversightRig(t)
	ctx := context.Background()
	lc := &plan.Lifecycle{Store: r.st}

	created, err := lc.Create(ctx, r.tenantID, minimalPlan("eval-gate-no-cases"), "test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := lc.ValidatePlan(ctx, r.tenantID, created.PlanID, created.Version); err != nil {
		t.Fatalf("ValidatePlan: %v", err)
	}

	g := &evals.Gate{} // nothing tagged for any artifact
	passed, _, err := lc.RunEvalGate(ctx, r.tenantID, created.PlanID, created.Version, evalgate.PlanGate(g, evals.GateInput{}))
	if err != nil {
		t.Fatalf("RunEvalGate: %v", err)
	}
	if passed {
		t.Fatal("a plan version with no cases tagged to it must not pass the eval gate")
	}

	_, status, err := lc.Get(ctx, r.tenantID, created.PlanID, created.Version)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if status != plan.StatusValidated {
		t.Fatalf("plan status after a failed gate = %s, want %s (fixable, re-runnable, not a dead end)", status, plan.StatusValidated)
	}
}
