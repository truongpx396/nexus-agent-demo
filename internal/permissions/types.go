// Package permissions is the 10-layer permission chain (README task 3.6,
// pattern 17): one published total order every tool invocation walks,
// verbatim from the table in README.md §4 —
//
//	1  Deny rules (tenant/tool/pattern)  -> DENY (final)
//	2  PreToolUse hooks                  -> DENY (final) | ASK | DEFER      (never ALLOW)
//	3  Autonomy level (pinned, ratchet)  -> DENY | ASK | DEFER
//	4  Gate 1: tool profile membership   -> DENY | DEFER                    (never ALLOW)
//	5  Gate 2: capability metadata       -> DENY | ASK | DEFER
//	6  Gate 3: per-invocation safety     -> DENY | ASK | DEFER   ALWAYS EVALUATED
//	7  Rule of Two (taint + declaration) -> ASK  | DEFER          ALWAYS EVALUATED
//	8  Approval policy                   -> AUTO | ASK(once|session|multi_party)
//	9  Standing scope / batch / preauth  -> SATISFIES an ASK      (never suppresses one)
//	10 Otherwise                         -> ALLOW
//
// Two invariants make the order load-bearing rather than decorative: a DENY
// at any layer is final (chain.go stops immediately, no bypass exists), and
// layers 6 and 7 are unconditional — every invocation runs them, even one an
// earlier layer would already resolve ALLOW-ish or a standing scope is known
// to satisfy, so "a standing scope cannot cause layer 6 or 7 to be skipped"
// (README.md §5's Phase 3 acceptance test) is a property of Resolve's loop
// structure, not of any one layer's logic.
package permissions

import "fmt"

// Decision is the chain's total vocabulary. Allow only ever appears as
// Resolution.Decision — no LayerOutcome from layers 1-8 may set it (Resolve
// treats one that does as a bug: see chain.go's guard).
type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
	Defer Decision = "defer" // not final — "this layer has no opinion; continue"
)

// AskKind is layer 8's refinement of an Ask outcome — what shape of human
// decision it should become once oversight (Phase 5) exists to act on it.
type AskKind string

const (
	AskNone       AskKind = ""
	AskOnce       AskKind = "once"
	AskSession    AskKind = "session"
	AskMultiParty AskKind = "multi_party"
)

// Layer names which of the 10 positions produced the chain's binding
// decision — carried onto Resolution so a denial's audit trail says WHERE
// it was refused, matching the Phase 3 demo line "refused at layer 3."
type Layer int

const (
	LayerDenyRules Layer = iota + 1
	LayerPreToolUseHooks
	LayerAutonomy
	LayerGate1Profile
	LayerGate2Capability
	LayerGate3Safety
	LayerRuleOfTwo
	LayerApprovalPolicy
	LayerStandingScope
	LayerFallbackAllow
)

func (l Layer) String() string {
	names := [...]string{
		"", "deny_rules", "pre_tool_use_hooks", "autonomy", "gate1_tool_profile",
		"gate2_capability", "gate3_safety", "rule_of_two", "approval_policy",
		"standing_scope", "fallback_allow",
	}
	if int(l) < 0 || int(l) >= len(names) {
		return fmt.Sprintf("layer(%d)", int(l))
	}
	return names[l]
}

// EffectClass mirrors internal/tools.EffectClass without importing that
// package — internal/permissions is a leaf with respect to internal/tools
// (tools depends on permissions, never the reverse), so the chain's
// vocabulary for what a tool DOES is declared locally and the pipeline
// copies a tool's descriptor into this shape at the call site
// (internal/tools/pipeline.go).
type EffectClass string

const (
	EffectClassReadOnly EffectClass = "read_only"
	EffectClassMutating EffectClass = "mutating"
	EffectClassExternal EffectClass = "external"
)

// Taint mirrors internal/tools.Taint for the same reason EffectClass does.
type Taint struct {
	ReturnsUntrusted bool
	ReadsPrivateData bool
	MutatesExternal  bool
}

// LayerOutcome is one layer's opinion. Decision must be Deny, Ask, or Defer
// — never Allow; Resolve enforces this at every call site rather than
// trusting each layer function to self-police it.
type LayerOutcome struct {
	Decision Decision
	Reason   string
	AskKind  AskKind // meaningful only when Decision == Ask
}

// Resolution is the chain's answer for one invocation.
type Resolution struct {
	Decision Decision
	Layer    Layer
	Reason   string
	AskKind  AskKind

	// UpdatedInput is non-nil when layer 2 (PreToolUse hooks) validly
	// rewrote the tool's input — the pipeline recomputes the canonical
	// digest from this before proceeding (README task 3.11's "re-binds the
	// digest").
	UpdatedInput []byte
}
