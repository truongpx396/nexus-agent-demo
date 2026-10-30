package plan_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/plan"
)

var tenant = uuid.New()

// validPlan is the smallest plan Validate accepts: one condition step that ends
// the run.
func validPlan() plan.Plan {
	return plan.Plan{
		Name: "p", StartStep: "start",
		Steps: []plan.Step{{ID: "start", Kind: plan.StepCondition, Condition: &plan.ConditionConfig{}}},
	}
}

func passGate(context.Context, plan.Plan) (bool, string, error) { return true, "ok", nil }
func failGate(context.Context, plan.Plan) (bool, string, error) { return false, "regressed", nil }

// advanceTo walks a fresh plan through the lifecycle up to (and including) to.
func advanceTo(t *testing.T, l *plan.Lifecycle, to plan.Status) plan.Plan {
	t.Helper()
	ctx := context.Background()
	p, err := l.Create(ctx, tenant, validPlan(), "alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if to == plan.StatusDraft {
		return p
	}
	steps := []struct {
		reach plan.Status
		do    func() error
	}{
		{plan.StatusValidated, func() error { return l.ValidatePlan(ctx, tenant, p.PlanID, 1) }},
		{plan.StatusEvalPassed, func() error { _, _, err := l.RunEvalGate(ctx, tenant, p.PlanID, 1, passGate); return err }},
		{plan.StatusSignedOff, func() error { return l.SignOff(ctx, tenant, p.PlanID, 1, "bob") }},
		{plan.StatusEnabled, func() error { return l.Enable(ctx, tenant, p.PlanID, 1, 7, "model-x") }},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("advancing to %s: %v", s.reach, err)
		}
		if s.reach == to {
			break
		}
	}
	return p
}

func TestLifecycle_AdvancesOnlyInOrderAndEnabledIsImmutable(t *testing.T) {
	f := newFakeStore()
	l := &plan.Lifecycle{Store: f}
	ctx := context.Background()
	p := advanceTo(t, l, plan.StatusDraft)

	// Skipping a stage is refused, and nothing moves.
	if err := l.SignOff(ctx, tenant, p.PlanID, 1, "bob"); err == nil {
		t.Fatal("a draft plan was signed off without being validated and gated")
	}
	if _, status, _ := l.Get(ctx, tenant, p.PlanID, 1); status != plan.StatusDraft {
		t.Errorf("status = %s after a refused sign-off, want draft", status)
	}

	// Each stage is reachable, in order, from the one before it.
	for _, to := range []plan.Status{plan.StatusValidated, plan.StatusEvalPassed, plan.StatusSignedOff, plan.StatusEnabled} {
		fresh := &plan.Lifecycle{Store: newFakeStore()}
		got := advanceTo(t, fresh, to)
		if _, status, err := fresh.Get(ctx, tenant, got.PlanID, 1); err != nil || status != to {
			t.Errorf("walking to %s ended at %s (%v)", to, status, err)
		}
	}

	enabled := advanceTo(t, l, plan.StatusEnabled)
	if err := l.ValidatePlan(ctx, tenant, enabled.PlanID, 1); err == nil {
		t.Error("an enabled plan version was changed: enabled versions are immutable, an in-flight run must finish on the spec it started on")
	}
	// Any transition out of an enabled version is refused as immutability, not
	// merely as "wrong stage": the guard comes first.
	if err := l.SignOff(ctx, tenant, enabled.PlanID, 1, "mallory"); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("SignOff of an enabled plan = %v, want an immutability refusal", err)
	}
	if err := l.Retire(ctx, tenant, enabled.PlanID, 1); err != nil {
		t.Errorf("Retire of an enabled plan: %v", err)
	}
	if err := l.Retire(ctx, tenant, enabled.PlanID, 1); err == nil {
		t.Error("a retired plan was retired again")
	}
	if err := l.Retire(ctx, tenant, p.PlanID, 1); err == nil {
		t.Error("a plan that was never enabled was retired")
	}
}

func TestLifecycle_AFailedEvalGateLeavesThePlanValidatedAndFixable(t *testing.T) {
	f := newFakeStore()
	l := &plan.Lifecycle{Store: f}
	ctx := context.Background()
	p := advanceTo(t, l, plan.StatusValidated)

	passed, detail, err := l.RunEvalGate(ctx, tenant, p.PlanID, 1, failGate)
	if err != nil || passed || detail != "regressed" {
		t.Fatalf("RunEvalGate = %v, %q, %v, want a reported failure, not an error", passed, detail, err)
	}
	if _, status, _ := l.Get(ctx, tenant, p.PlanID, 1); status != plan.StatusValidated {
		t.Errorf("status = %s after a failed gate, want validated: never a dead end", status)
	}
	if passed, _, err := l.RunEvalGate(ctx, tenant, p.PlanID, 1, passGate); err != nil || !passed {
		t.Errorf("re-running the gate = %v, %v, want it to pass and advance", passed, err)
	}
}

func TestLifecycle_ValidationRefusesAnInvalidPlan(t *testing.T) {
	l := &plan.Lifecycle{Store: newFakeStore()}
	ctx := context.Background()
	bad, err := l.Create(ctx, tenant, plan.Plan{Name: "empty", StartStep: "nowhere"}, "alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := l.ValidatePlan(ctx, tenant, bad.PlanID, 1); err == nil {
		t.Error("an invalid plan advanced to validated")
	}
	if _, status, _ := l.Get(ctx, tenant, bad.PlanID, 1); status != plan.StatusDraft {
		t.Errorf("status = %s, want it still draft", status)
	}
}

func TestLifecycle_ANewVersionStartsAsDraftAndTheLatestIsFound(t *testing.T) {
	l := &plan.Lifecycle{Store: newFakeStore()}
	ctx := context.Background()
	p := advanceTo(t, l, plan.StatusEnabled)

	v2, err := l.NewVersion(ctx, tenant, p.PlanID, validPlan(), "alice")
	if err != nil || v2.Version != 2 {
		t.Fatalf("NewVersion = %+v, %v, want version 2", v2, err)
	}
	if latest, status, err := l.Get(ctx, tenant, p.PlanID, 0); err != nil || latest.Version != 2 || status != plan.StatusDraft {
		t.Errorf("latest = v%d %s (%v), want v2 draft: a new version never inherits the prior one's progress", latest.Version, status, err)
	}
	if _, _, err := l.Get(ctx, tenant, uuid.New(), 0); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Get of an unknown plan = %v, want not found", err)
	}
}

// Sign-off and enablement used to write their governance record in a second
// transaction after the status change, so a crash between them left a plan
// signed off or enabled with no record of by whom or with what. They are one
// unit now.
func TestLifecycle_TheGovernanceRecordCommitsWithTheStatus(t *testing.T) {
	t.Run("sign-off", func(t *testing.T) {
		f := newFakeStore()
		l := &plan.Lifecycle{Store: f}
		p := advanceTo(t, l, plan.StatusEvalPassed)
		f.failSignedOff = errors.New("database unavailable")

		if err := l.SignOff(context.Background(), tenant, p.PlanID, 1, "bob"); err == nil {
			t.Fatal("SignOff succeeded though the signer could not be recorded")
		}
		if got := f.plans[planKey{p.PlanID, 1}].status; got != plan.StatusEvalPassed {
			t.Errorf("status = %s, want it still eval_passed: a plan signed off by no one is not signed off", got)
		}

		f.failSignedOff = nil
		if err := l.SignOff(context.Background(), tenant, p.PlanID, 1, "bob"); err != nil {
			t.Fatalf("SignOff retry: %v", err)
		}
		if row := f.plans[planKey{p.PlanID, 1}]; row.status != plan.StatusSignedOff || row.signedOffBy != "bob" {
			t.Errorf("plan = %s signed off by %q, want signed_off by bob", row.status, row.signedOffBy)
		}
	})
	t.Run("enable", func(t *testing.T) {
		f := newFakeStore()
		l := &plan.Lifecycle{Store: f}
		p := advanceTo(t, l, plan.StatusSignedOff)
		f.failEnable = errors.New("database unavailable")

		if err := l.Enable(context.Background(), tenant, p.PlanID, 1, 7, "model-x"); err == nil {
			t.Fatal("Enable succeeded though its pins could not be recorded")
		}
		if got := f.plans[planKey{p.PlanID, 1}].status; got != plan.StatusSignedOff {
			t.Errorf("status = %s, want it still signed_off: an enabled plan with no pins would run on whatever is current", got)
		}

		f.failEnable = nil
		if err := l.Enable(context.Background(), tenant, p.PlanID, 1, 7, "model-x"); err != nil {
			t.Fatalf("Enable retry: %v", err)
		}
		if row := f.plans[planKey{p.PlanID, 1}]; row.status != plan.StatusEnabled || row.agentPin != 7 || row.modelPin != "model-x" {
			t.Errorf("plan = %s pins %d/%q, want enabled with the pins recorded", row.status, row.agentPin, row.modelPin)
		}
	})
}
