package permissions

// TaintLeg is one of the three legs constitution Principle V counts: "at
// most two of {process untrusted input, access private data, change state
// or communicate externally} are permitted without human approval" (the
// Rule of Two, README task 3.10, pattern 21).
type TaintLeg int

const (
	LegUntrustedInput TaintLeg = iota
	LegPrivateData
	LegExternalEffect
)

// TaintState is one session's accumulated Rule-of-Two projection — which
// legs this session has engaged so far. A later phase rebuilds this by
// replaying taint_transition events (pattern 21's "session taint state as a
// projection"); this type is the pure value that replay would produce, kept
// free of the event log so it stays cheap to construct in a test.
type TaintState struct {
	Engaged [3]bool
}

func (s TaintState) engagedCount() int {
	n := 0
	for _, e := range s.Engaged {
		if e {
			n++
		}
	}
	return n
}

// Rebaseline clears ONLY the untrusted-input leg — the one leg an operator
// re-baseline is permitted to clear (pattern 21). The private-data and
// external-effect legs, once engaged, stay engaged for the life of the
// session; wiring an actual operator action to this method is a later
// phase's surface/oversight concern, not this one's.
func (s TaintState) Rebaseline() TaintState {
	s.Engaged[LegUntrustedInput] = false
	return s
}

func legsFor(t Taint) [3]bool {
	return [3]bool{t.ReturnsUntrusted, t.ReadsPrivateData, t.MutatesExternal}
}

// ResolveRuleOfTwo is layer 7, ALWAYS evaluated (README.md §4's chain
// table): admitting this call's declared legs into the session's running
// total is fine up to two distinct legs; a third resolves ASK, never DENY.
// The returned TaintState reflects the engagement whether or not the
// decision was ASK — Rule of Two counts what a session HAS DONE, and this
// call's taint is real regardless of whether the human ultimately approves
// it (an approval clears the ASK, not the historical fact that the leg was
// touched).
func ResolveRuleOfTwo(state TaintState, t Taint) (LayerOutcome, TaintState) {
	next := state
	for i, engaged := range legsFor(t) {
		if engaged {
			next.Engaged[i] = true
		}
	}
	if next.engagedCount() > 2 {
		return LayerOutcome{
			Decision: Ask,
			AskKind:  AskSession,
			Reason:   "this call would engage a third Rule-of-Two leg within the same session",
		}, next
	}
	return LayerOutcome{Decision: Defer, Reason: "within the two-leg Rule-of-Two budget"}, next
}
