package permissions

import "regexp"

// DenyRule is layer 1: a tenant/tool/pattern deny rule (README.md §4's chain
// table, row 1). It is the only layer that can be configured to deny by
// tool identity alone — everything below it must look at parsed input,
// autonomy, or accumulated session state.
type DenyRule struct {
	Name    string
	Tool    string         // "*", a bare namespace, "namespace/*", or an exact ToolRef string
	Pattern *regexp.Regexp // optional: also match the invocation's raw input JSON
	Reason  string
}

func (r DenyRule) matches(toolID, namespace, rawInput string) bool {
	if !matchGlob(r.Tool, toolID, namespace) {
		return false
	}
	if r.Pattern != nil && !r.Pattern.MatchString(rawInput) {
		return false
	}
	return true
}

// ResolveDenyRules is layer 1's evaluation: the first matching rule wins
// (deny rules are not layered against each other; the chain's total order
// already gives them the earliest and most final say).
func ResolveDenyRules(rules []DenyRule, toolID, namespace, rawInput string) LayerOutcome {
	for _, r := range rules {
		if r.matches(toolID, namespace, rawInput) {
			return LayerOutcome{Decision: Deny, Reason: "deny rule " + r.Name + ": " + r.Reason}
		}
	}
	return LayerOutcome{Decision: Defer, Reason: "no deny rule matched"}
}
