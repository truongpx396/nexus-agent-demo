package evals

// HeldOutPolicy bounds how far a visible-corpus pass rate may outrun a
// held-out one before it's treated as a spec-gaming signal (task 10.6).
type HeldOutPolicy struct {
	MaxGap float64
}

// DefaultHeldOutPolicy is deliberately generous — the point of measuring the
// gap is to make it visible on the golden-signal dashboard (task 10.12)
// before it needs to block anything; a tenant/CI config can tighten this.
var DefaultHeldOutPolicy = HeldOutPolicy{MaxGap: 0.15}

// PassRate is the fraction of trials in r that passed. Inconclusive counts
// against the rate, same as Report.Pass's own reading of it (FR-138: never
// silently a pass).
func (r Report) PassRate() float64 {
	if len(r.Trials) == 0 {
		return 0
	}
	passed := 0
	for _, t := range r.Trials {
		if t.Verdict == VerdictPass {
			passed++
		}
	}
	return float64(passed) / float64(len(r.Trials))
}

// HeldOutGap measures visible-vs-held-out pass-rate divergence (task 10.6):
// a WIDENING gap (visible outperforming held-out) is how spec-gaming
// announces itself — a change tuned against the graders the agent can see
// looks great on the visible corpus and does worse on the ones it can't.
// A negative gap (held-out doing BETTER) is not a finding this function
// flags; it clamps to 0, since that direction isn't the failure mode task
// 10.6 names.
func HeldOutGap(visible, heldOut Report) float64 {
	gap := visible.PassRate() - heldOut.PassRate()
	if gap < 0 {
		return 0
	}
	return gap
}

// CheckHeldOutGap reports the measured gap and whether it clears policy.
func CheckHeldOutGap(visible, heldOut Report, policy HeldOutPolicy) (gap float64, within bool) {
	gap = HeldOutGap(visible, heldOut)
	return gap, gap <= policy.MaxGap
}
