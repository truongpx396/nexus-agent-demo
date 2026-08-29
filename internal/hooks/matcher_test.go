package hooks

import "testing"

func TestMatchesTool(t *testing.T) {
	cases := []struct {
		pattern, toolID, namespace string
		want                       bool
	}{
		{"*", "platform/shell@v1", "platform", true},
		{"", "platform/shell@v1", "platform", true},
		{"platform/shell@v1", "platform/shell@v1", "platform", true},
		{"platform/*", "platform/shell@v1", "platform", true},
		{"platform/*", "acme/custom@v1", "acme", false},
		{"platform", "platform/shell@v1", "platform", true},
		{"acme", "platform/shell@v1", "platform", false},
	}
	for _, tc := range cases {
		got := matchesTool(tc.pattern, tc.toolID, tc.namespace)
		if got != tc.want {
			t.Errorf("matchesTool(%q, %q, %q) = %v, want %v", tc.pattern, tc.toolID, tc.namespace, got, tc.want)
		}
	}
}

func TestExprEval(t *testing.T) {
	fields := map[string]string{"autonomy_level": "supervised", "effect_class": "mutating"}

	cases := []struct {
		name string
		expr Expr
		want bool
	}{
		{"zero value matches everything", Expr{}, true},
		{"eq true", Expr{Eq: &EqExpr{Field: "autonomy_level", Value: "supervised"}}, true},
		{"eq false", Expr{Eq: &EqExpr{Field: "autonomy_level", Value: "read_only"}}, false},
		{"in true", Expr{In: &InExpr{Field: "effect_class", Values: []string{"mutating", "external"}}}, true},
		{"in false", Expr{In: &InExpr{Field: "effect_class", Values: []string{"read_only"}}}, false},
		{"not", Expr{Not: &Expr{Eq: &EqExpr{Field: "autonomy_level", Value: "read_only"}}}, true},
		{
			"and both true", Expr{And: []Expr{
				{Eq: &EqExpr{Field: "autonomy_level", Value: "supervised"}},
				{Eq: &EqExpr{Field: "effect_class", Value: "mutating"}},
			}}, true,
		},
		{
			"and one false", Expr{And: []Expr{
				{Eq: &EqExpr{Field: "autonomy_level", Value: "supervised"}},
				{Eq: &EqExpr{Field: "effect_class", Value: "read_only"}},
			}}, false,
		},
		{
			"or one true", Expr{Or: []Expr{
				{Eq: &EqExpr{Field: "autonomy_level", Value: "read_only"}},
				{Eq: &EqExpr{Field: "effect_class", Value: "mutating"}},
			}}, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.expr.Eval(fields); got != tc.want {
				t.Errorf("Eval() = %v, want %v", got, tc.want)
			}
		})
	}
}
