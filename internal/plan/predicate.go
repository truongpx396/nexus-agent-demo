package plan

import "fmt"

// ValueKind is a Value's type tag — comparisons across kinds are a
// validation error, never a silent coercion (README task 8.2: "typed field
// refs").
type ValueKind string

const (
	KindString ValueKind = "string"
	KindNumber ValueKind = "number"
	KindBool   ValueKind = "bool"
)

// Value is a typed predicate operand — a JSON scalar tagged with which of
// its fields is meaningful, so (for example) the number 0 and the string
// "0" are never accidentally equal.
type Value struct {
	Kind ValueKind `json:"kind"`
	Str  string    `json:"str,omitempty"`
	Num  float64   `json:"num,omitempty"`
	Bool bool      `json:"bool,omitempty"`
}

func StringValue(s string) Value  { return Value{Kind: KindString, Str: s} }
func NumberValue(n float64) Value { return Value{Kind: KindNumber, Num: n} }
func BoolValue(b bool) Value      { return Value{Kind: KindBool, Bool: b} }

// Equal compares two Values — different kinds are never equal (rather than
// an error), so eq/ne stay total functions over any two well-formed Values.
func (v Value) Equal(o Value) bool {
	if v.Kind != o.Kind {
		return false
	}
	switch v.Kind {
	case KindString:
		return v.Str == o.Str
	case KindNumber:
		return v.Num == o.Num
	case KindBool:
		return v.Bool == o.Bool
	default:
		return false
	}
}

// less is used by lt/gt — only defined for KindNumber; a type mismatch or a
// non-ordered kind is a validation-time error (validate.go), never
// evaluated silently as false.
func (v Value) less(o Value) (bool, error) {
	if v.Kind != KindNumber || o.Kind != KindNumber {
		return false, fmt.Errorf("plan: lt/gt only compares numbers, got %s and %s", v.Kind, o.Kind)
	}
	return v.Num < o.Num, nil
}

// Op is the closed set of predicate operators (README task 8.2): eq, ne,
// lt, gt, and, or, in — no string eval, no I/O, no model call, no unbounded
// loop. That closure is what makes the zero-token routing claim a property
// of the type rather than of caller discipline.
type Op string

const (
	OpEq  Op = "eq"
	OpNe  Op = "ne"
	OpLt  Op = "lt"
	OpGt  Op = "gt"
	OpAnd Op = "and"
	OpOr  Op = "or"
	OpIn  Op = "in"
)

// Predicate is the closed JSON AST (README task 8.2). Field is a dot-free
// key into a Context map (this demo's step outputs are flat; a dotted path
// is future work, not a Phase 8 requirement) — used by eq/ne/lt/gt/in.
// And/Or hold nested Predicates; every other field is ignored for those two
// ops. maxPredicateDepth (validate.go) bounds how deep Args may nest.
type Predicate struct {
	Op     Op          `json:"op"`
	Field  string      `json:"field,omitempty"`
	Value  Value       `json:"value,omitempty"`
	Values []Value     `json:"values,omitempty"`
	And    []Predicate `json:"and,omitempty"`
	Or     []Predicate `json:"or,omitempty"`
}

// Context is the flat variable set a plan run has accumulated so far —
// step OutputVar/ResultVar/CounterVar bindings. Eval reads it; nothing in
// this package ever writes to it, or to any external system: the whole
// point of a closed predicate AST is that Eval cannot have a side effect.
type Context map[string]Value

// Eval resolves p against ctx with ZERO I/O and ZERO model calls (README
// task 8.6's own property test asserts exactly this at the Executor level,
// by counting Provider.Stream calls during transition evaluation — Eval
// itself has no way to make one: it takes no Provider, no context.Context,
// no channel, nothing but ctx and a Predicate — an unbounded loop is
// likewise structurally impossible, since And/Or only ever recurse over a
// slice already fully materialized in memory).
func (p Predicate) Eval(ctx Context) (bool, error) {
	switch p.Op {
	case OpAnd:
		for _, sub := range p.And {
			ok, err := sub.Eval(ctx)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	case OpOr:
		for _, sub := range p.Or {
			ok, err := sub.Eval(ctx)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case OpEq, OpNe, OpLt, OpGt, OpIn:
		v, ok := ctx[p.Field]
		if !ok {
			return false, fmt.Errorf("plan: predicate references unbound field %q", p.Field)
		}
		switch p.Op { //nolint:exhaustive // this inner switch only ever sees the 5 leaf ops the outer case already narrowed to (OpAnd/OpOr are handled above, before this switch is reached)
		case OpEq:
			return v.Equal(p.Value), nil
		case OpNe:
			return !v.Equal(p.Value), nil
		case OpLt:
			return v.less(p.Value)
		case OpGt:
			return p.Value.less(v)
		case OpIn:
			for _, cand := range p.Values {
				if v.Equal(cand) {
					return true, nil
				}
			}
			return false, nil
		}
	}
	return false, fmt.Errorf("plan: unrecognized predicate op %q", p.Op)
}
