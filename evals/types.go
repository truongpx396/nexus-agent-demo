// Package evals is the release gate (docs/constitution.md, Principle IX):
// it exists — and is wired into CI — before the first behavior-bearing
// slice ships, not after, because the window in which changes have the
// largest effect sizes is otherwise the window in which they go
// unmeasured.
//
// Phase 1 ships the schema and the mechanics against the one piece of real
// behavior that exists this early: the deterministic fake provider
// (internal/provider/fake). Phase 2 onward adds suites that exercise the
// kernel itself through the same Case/Trial/Report shapes — the harness
// does not get rewritten when there is finally an agent to grade.
package evals

// Class distinguishes how strictly a case's verdict is allowed to be read.
// A safety case admits no threshold below 100% pass (Phase 9); Phase 1
// stores the class on every case now so that rule has something to read
// later without a schema change.
type Class string

const (
	ClassRegression Class = "regression"
	ClassCapability Class = "capability"
	ClassSafety     Class = "safety"
	ClassNegative   Class = "negative"
)

// Verdict is three-valued on purpose: Inconclusive must never silently
// resolve to Pass (FR-138) — grading is what protects that invariant, but
// the type exists so nothing can accidentally collapse the distinction
// with a bool.
type Verdict string

const (
	VerdictPass         Verdict = "pass"
	VerdictFail         Verdict = "fail"
	VerdictInconclusive Verdict = "inconclusive"
)

// Trial is the outcome of running one case once. Phase 9 adds k-trials-per-
// case statistics on top of this same shape; Phase 1 runs exactly one trial
// per case because there is no live-model variance yet to average over.
type Trial struct {
	CaseID  string
	Verdict Verdict
	Detail  string // human-readable — never itself a graded signal
}

// Report is every trial from one gate run.
type Report struct {
	Trials []Trial
}

// Pass reports whether every trial passed. Inconclusive counts as a
// failure here — never as a pass — per FR-138.
func (r Report) Pass() bool {
	for _, t := range r.Trials {
		if t.Verdict != VerdictPass {
			return false
		}
	}
	return true
}
