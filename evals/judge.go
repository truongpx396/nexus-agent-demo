package evals

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// BudgetGate is the narrow slice of kernel.BudgetGate a Judge needs — just
// enough to reserve before every model call, never more (the same
// declare-only-what-you-call idiom internal/tools/builtin uses for
// SkillResolver/SkillEvents). internal/cost.Gate satisfies this
// structurally; so does the package-local noopBudgetGate below.
type BudgetGate interface {
	Reserve(ctx context.Context, req cost.ReserveRequest) (cost.Reservation, error)
}

// noopBudgetGate is what an unconfigured Judge reserves against — every
// call resolves cost.DecisionSkip, mirroring kernel.NoopBudgetGate's own
// reasoning (evals stays a leaf with respect to kernel, so this is
// reimplemented here rather than imported) without silently becoming
// unmetered: "off the paying loop" must still mean a Purpose-tagged
// Reserve call happened, per README task 4.8 and
// tests/contract/cost_metering_test.go's AST enforcement of it.
type noopBudgetGate struct{}

func (noopBudgetGate) Reserve(_ context.Context, req cost.ReserveRequest) (cost.Reservation, error) {
	return cost.Reservation{
		ID: uuid.New(), TenantID: req.TenantID, SessionID: req.SessionID, ModelID: req.ModelID,
		Decision: cost.Decision{Kind: cost.DecisionSkip, Reason: "evals.Judge: no BudgetGate configured"},
	}, nil
}

// Judge is the last-resort grader (README task 10.4: "the judge reserved
// for genuinely subjective criteria") — everything this package can check
// with code (provider_case.go, permission_case.go, admission_case.go) never
// reaches this file. A Judge is a PINNED snapshot of a model from a
// DIFFERENT family than the one being graded (task 10.5's "cross-family"),
// so the judge is never asked to rubber-stamp its own family's stylistic
// habits, and it is calibrated against human labels before it may block
// anything.
type Judge struct {
	Provider provider.Provider
	// Budget reserves before every call this Judge makes (README task 4.8:
	// "off the paying loop" = a cheaper meter, never unmetered) — nil
	// defaults to noopBudgetGate, which still reserves, just resolves
	// DecisionSkip every time (the honest default until a caller wires a
	// real internal/cost.Gate in, the same posture kernel.NoopBudgetGate
	// takes for the main turn loop before Phase 4 wires the real one).
	Budget BudgetGate
	// PinnedModelID names the exact snapshot this Judge is bound to —
	// changing it is a deploy (constitution Principle IX: "a prompt or
	// model change is a deploy"), never a silent drift to "whatever the
	// alias currently resolves to."
	PinnedModelID string
	// AgreementFloor is the minimum agreement rate against human labels
	// Calibrate must clear before Grade is trusted to block anything (task
	// 10.5: "calibrated ... to a published agreement floor before it may
	// block a change").
	AgreementFloor float64

	calibrated        bool
	measuredAgreement float64
}

// JudgeCase is one genuinely subjective grading task: a rubric, not an
// exact-match Want like ProviderScriptCase's. WantVerdict is the human
// label used only during Calibrate; Grade never reads it.
type JudgeCase struct {
	ID          string
	Class       Class
	Description string
	Prompt      string // what the judge is shown to grade
	Rubric      string // the criterion — deliberately prose, because this is the class of case where prose IS the spec
	WantVerdict Verdict
}

// Calibrate runs Grade (bypassing the calibration gate, since that's what
// this call IS establishing) against a labeled set and records the
// agreement rate. A Judge that has never been calibrated, or fell below
// AgreementFloor the last time it was, refuses to grade (Grade returns
// VerdictInconclusive, never a silent Pass) — the same fail-closed instinct
// permissions/safety.Classifier applies to an unavailable model leg, aimed
// here at an uncalibrated one instead of an unreachable one.
func (j *Judge) Calibrate(ctx context.Context, labeled []JudgeCase) (agreement float64, err error) {
	if len(labeled) == 0 {
		return 0, fmt.Errorf("evals: Calibrate requires at least one labeled case")
	}
	agree := 0
	for _, c := range labeled {
		v, _, gerr := j.gradeOnce(ctx, c)
		if gerr != nil {
			return 0, fmt.Errorf("evals: calibration case %s: %w", c.ID, gerr)
		}
		if v == c.WantVerdict {
			agree++
		}
	}
	j.measuredAgreement = float64(agree) / float64(len(labeled))
	j.calibrated = j.measuredAgreement >= j.AgreementFloor
	return j.measuredAgreement, nil
}

// Calibrated reports whether the last Calibrate run cleared AgreementFloor.
func (j *Judge) Calibrated() bool { return j.calibrated }

// gradeOnce asks the provider to grade one case and parses its verdict —
// shared by Calibrate (against a known label) and Grade (blind).
func (j *Judge) gradeOnce(ctx context.Context, c JudgeCase) (Verdict, string, error) {
	if j.Provider == nil {
		return "", "", fmt.Errorf("evals: Judge has no Provider configured")
	}
	budget := j.Budget
	if budget == nil {
		budget = noopBudgetGate{}
	}
	if _, err := budget.Reserve(ctx, cost.ReserveRequest{ModelID: j.PinnedModelID, Purpose: cost.PurposeJudge}); err != nil {
		return "", "", fmt.Errorf("judge reserve: %w", err)
	}

	prompt := provider.Prompt{
		System:   fmt.Sprintf("You are a calibrated grading judge. Rubric: %s\nAnswer only PASS or FAIL, then a one-line reason.", c.Rubric),
		Messages: []provider.Message{{Role: "user", Text: c.Prompt}},
	}
	// PinnedModelID is this Judge's identity for the digest/audit trail, not
	// a per-call routing knob — like internal/provider/anthropic, a
	// concrete Provider is already bound to one model; Stream's signature
	// (internal/provider/provider.go) carries no per-call model override.
	stream, err := j.Provider.Stream(ctx, prompt, nil, provider.RunContext{})
	if err != nil {
		return "", "", fmt.Errorf("judge stream: %w", err)
	}
	var text string
	for {
		chunk, ok, serr := stream.Next(ctx)
		if serr != nil {
			return "", "", fmt.Errorf("judge stream: %w", serr)
		}
		if !ok {
			break
		}
		if chunk.Kind == provider.ChunkContent {
			text += chunk.Text
		}
	}
	pass := parseJudgeVerdict(text)
	if pass {
		return VerdictPass, text, nil
	}
	return VerdictFail, text, nil
}

// parseJudgeVerdict is a deliberately narrow parse: the prompt above asks
// for exactly one of two tokens, so this looks for "PASS" case-insensitively
// as the first non-whitespace word rather than doing any NLP — the fake
// provider used in every correctness test scripts its content verbatim, so
// this only ever sees exactly what a case's fixture put there.
func parseJudgeVerdict(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] == ' ' || text[i] == '\n' || text[i] == '\t' {
			continue
		}
		return len(text) >= i+4 && (text[i:i+4] == "PASS" || text[i:i+4] == "Pass" || text[i:i+4] == "pass")
	}
	return false
}

// Grade grades one case. A Judge that isn't calibrated refuses to grade at
// all — VerdictInconclusive, never a guess dressed up as a pass (task
// 10.5's "before it may block a change" made literal).
func (j *Judge) Grade(ctx context.Context, c JudgeCase) Trial {
	if !j.calibrated {
		return Trial{CaseID: c.ID, Verdict: VerdictInconclusive, Detail: fmt.Sprintf("judge not calibrated (last measured agreement %.2f, floor %.2f) — refusing to grade rather than guess", j.measuredAgreement, j.AgreementFloor)}
	}
	v, detail, err := j.gradeOnce(ctx, c)
	if err != nil {
		return Trial{CaseID: c.ID, Verdict: VerdictInconclusive, Detail: err.Error()}
	}
	return Trial{CaseID: c.ID, Verdict: v, Detail: detail}
}

// RunJudgeCases grades every case with j, in order.
func RunJudgeCases(ctx context.Context, j *Judge, cases []JudgeCase) Report {
	var report Report
	for _, c := range cases {
		report.Trials = append(report.Trials, j.Grade(ctx, c))
	}
	return report
}
