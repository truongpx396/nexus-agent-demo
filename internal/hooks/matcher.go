package hooks

import "strings"

// matchesTool reports whether pattern selects toolID/namespace. "*" or ""
// matches everything; "ns/*" matches every tool in namespace ns; anything
// else must equal either the full tool ref or the bare namespace exactly.
func matchesTool(pattern, toolID, namespace string) bool {
	switch {
	case pattern == "" || pattern == "*":
		return true
	case pattern == toolID:
		return true
	case strings.HasSuffix(pattern, "/*"):
		return strings.TrimSuffix(pattern, "/*") == namespace
	default:
		return pattern == namespace
	}
}

// Expr is a closed JSON-AST boolean predicate (README.md §3 pattern 52's
// "predicates are a closed JSON AST, not a string language" discipline,
// reused here for hook conditions) — exactly one field set per node.
// Marshaling to/from JSON is what a tenant-config-driven hook definition
// (a future phase's internal/config) would store; this package only needs
// to evaluate the tree, so no JSON tags are required to be exhaustive here.
type Expr struct {
	Eq  *EqExpr `json:"eq,omitempty"`
	In  *InExpr `json:"in,omitempty"`
	And []Expr  `json:"and,omitempty"`
	Or  []Expr  `json:"or,omitempty"`
	Not *Expr   `json:"not,omitempty"`
}

// EqExpr matches when fields[Field] == Value.
type EqExpr struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// InExpr matches when fields[Field] is one of Values.
type InExpr struct {
	Field  string   `json:"field"`
	Values []string `json:"values"`
}

// Eval evaluates the tree against a flat field map (Context.fields). A zero
// Expr (no field set) evaluates true — the same "nil/empty condition means
// unconditional" rule Config.If's doc comment states.
func (e Expr) Eval(fields map[string]string) bool {
	switch {
	case e.Eq != nil:
		return fields[e.Eq.Field] == e.Eq.Value
	case e.In != nil:
		for _, v := range e.In.Values {
			if fields[e.In.Field] == v {
				return true
			}
		}
		return false
	case e.Not != nil:
		return !e.Not.Eval(fields)
	case len(e.And) > 0:
		for _, sub := range e.And {
			if !sub.Eval(fields) {
				return false
			}
		}
		return true
	case len(e.Or) > 0:
		for _, sub := range e.Or {
			if sub.Eval(fields) {
				return true
			}
		}
		return false
	default:
		return true
	}
}
