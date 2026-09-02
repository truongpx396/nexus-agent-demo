package evals

import "math"

// DefaultZ is the z-score for a 95% Wilson confidence interval — the
// interval width every case verdict and every regression check in this
// package is computed against, unless a caller has a specific reason to
// override it.
const DefaultZ = 1.96

// Interval is a closed confidence interval over an observed pass rate,
// [Low, High] ⊆ [0, 1]. README task 10.2 requires "exact intervals" (a
// Wilson score interval, not the normal approximation, which undercovers at
// small n — exactly the n a per-case eval trial count usually is) and
// requires regression to be defined as INTERVAL SEPARATION, never as "a
// trial that used to pass now fails": a single flipped trial out of k is
// exactly the noise an interval is computed to absorb.
type Interval struct {
	Low, High float64
}

// Width reports how wide the interval is — a useful sanity signal
// separately from Low/High: a very wide interval (too few trials) means
// "inconclusive" is the honest answer even before comparing to a threshold.
func (iv Interval) Width() float64 { return iv.High - iv.Low }

// Separated reports whether iv is strictly, entirely below other — the
// definition of "candidate regressed against baseline" this package uses
// throughout (task 10.2): not "iv's point estimate is lower," which a
// single flipped trial can produce by chance, but "the two intervals do not
// even overlap."
func (iv Interval) Separated(other Interval) bool {
	return iv.High < other.Low
}

// WilsonInterval computes the Wilson score interval for successes out of n
// trials at confidence z (DefaultZ for the package's standard 95%). n == 0
// returns the maximally uncertain interval [0, 1] — no trials ran, so
// nothing can be asserted about the pass rate; EvaluateCase reads this back
// out as Inconclusive rather than a lucky Pass.
func WilsonInterval(successes, n int, z float64) Interval {
	if n <= 0 {
		return Interval{Low: 0, High: 1}
	}
	nf := float64(n)
	phat := float64(successes) / nf
	z2 := z * z

	denom := 1 + z2/nf
	center := phat + z2/(2*nf)
	margin := z * math.Sqrt(phat*(1-phat)/nf+z2/(4*nf*nf))

	low := (center - margin) / denom
	high := (center + margin) / denom
	return Interval{Low: clamp01(low), High: clamp01(high)}
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// RunKTrials runs a case k times and collects every Trial. Every grader in
// this package is deterministic today (the fake provider, the permission
// chain, the admission scanners — nothing in this codebase's test harness
// has live-model variance yet), so k identical deterministic trials collapse
// WilsonInterval to a single point; the mechanism is still real and is
// exactly what starts producing a genuinely wide interval the day a case's
// run func calls a non-deterministic judge (judge.go) instead — the same
// "ship the seam before the variance exists to fill it" choice this
// package's own design already makes for its class thresholds (policy.go).
func RunKTrials(k int, run func(trial int) Trial) []Trial {
	if k < 1 {
		k = 1
	}
	trials := make([]Trial, k)
	for i := 0; i < k; i++ {
		trials[i] = run(i)
	}
	return trials
}

// classifyTrials tallies trials into passed/failed/indeterminate counts —
// the shared helper EvaluateCase builds its verdict from. Indeterminate is
// counted separately from failed on purpose: a trial the grader itself
// couldn't resolve (VerdictInconclusive — an uncalibrated judge, a grader
// error) is evidence of nothing, not evidence of a defect, so it must never
// silently count toward "this failed."
func classifyTrials(trials []Trial) (passed, failed, indeterminate, n int) {
	n = len(trials)
	for _, t := range trials {
		switch t.Verdict {
		case VerdictPass:
			passed++
		case VerdictFail:
			failed++
		case VerdictInconclusive:
			indeterminate++
		default:
			indeterminate++
		}
	}
	return passed, failed, indeterminate, n
}
