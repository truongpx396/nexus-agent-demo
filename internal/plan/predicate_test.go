package plan

import "testing"

func TestPredicate_EqNe(t *testing.T) {
	ctx := Context{"category": StringValue("urgent")}
	eq := Predicate{Op: OpEq, Field: "category", Value: StringValue("urgent")}
	if ok, err := eq.Eval(ctx); err != nil || !ok {
		t.Fatalf("eq urgent==urgent = %v, %v; want true, nil", ok, err)
	}
	ne := Predicate{Op: OpNe, Field: "category", Value: StringValue("low")}
	if ok, err := ne.Eval(ctx); err != nil || !ok {
		t.Fatalf("ne urgent!=low = %v, %v; want true, nil", ok, err)
	}
}

func TestPredicate_DifferentKindsNeverEqual(t *testing.T) {
	ctx := Context{"n": NumberValue(0)}
	p := Predicate{Op: OpEq, Field: "n", Value: StringValue("0")}
	ok, err := p.Eval(ctx)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if ok {
		t.Fatalf("number 0 and string \"0\" compared equal — typed field refs must never coerce across kinds")
	}
}

func TestPredicate_LtGt(t *testing.T) {
	ctx := Context{"score": NumberValue(5)}
	if ok, err := (Predicate{Op: OpLt, Field: "score", Value: NumberValue(10)}).Eval(ctx); err != nil || !ok {
		t.Fatalf("5 < 10 = %v, %v; want true, nil", ok, err)
	}
	if ok, err := (Predicate{Op: OpGt, Field: "score", Value: NumberValue(1)}).Eval(ctx); err != nil || !ok {
		t.Fatalf("5 > 1 = %v, %v; want true, nil", ok, err)
	}
	if ok, err := (Predicate{Op: OpLt, Field: "score", Value: NumberValue(1)}).Eval(ctx); err != nil || ok {
		t.Fatalf("5 < 1 = %v, %v; want false, nil", ok, err)
	}
}

func TestPredicate_LtGt_RefusesNonNumberOperands(t *testing.T) {
	ctx := Context{"s": StringValue("a")}
	if _, err := (Predicate{Op: OpLt, Field: "s", Value: NumberValue(1)}).Eval(ctx); err == nil {
		t.Fatalf("lt over a string field should error, not silently return false")
	}
}

func TestPredicate_In(t *testing.T) {
	ctx := Context{"status": StringValue("blocked")}
	p := Predicate{Op: OpIn, Field: "status", Values: []Value{StringValue("open"), StringValue("blocked")}}
	if ok, err := p.Eval(ctx); err != nil || !ok {
		t.Fatalf("in = %v, %v; want true, nil", ok, err)
	}
	p2 := Predicate{Op: OpIn, Field: "status", Values: []Value{StringValue("done")}}
	if ok, err := p2.Eval(ctx); err != nil || ok {
		t.Fatalf("in (no match) = %v, %v; want false, nil", ok, err)
	}
}

func TestPredicate_AndOr(t *testing.T) {
	ctx := Context{"a": BoolValue(true), "b": BoolValue(false)}
	and := Predicate{Op: OpAnd, And: []Predicate{
		{Op: OpEq, Field: "a", Value: BoolValue(true)},
		{Op: OpEq, Field: "b", Value: BoolValue(true)},
	}}
	if ok, err := and.Eval(ctx); err != nil || ok {
		t.Fatalf("and(true,false) = %v, %v; want false, nil", ok, err)
	}
	or := Predicate{Op: OpOr, Or: []Predicate{
		{Op: OpEq, Field: "a", Value: BoolValue(true)},
		{Op: OpEq, Field: "b", Value: BoolValue(true)},
	}}
	if ok, err := or.Eval(ctx); err != nil || !ok {
		t.Fatalf("or(true,false) = %v, %v; want true, nil", ok, err)
	}
}

func TestPredicate_UnboundFieldErrors(t *testing.T) {
	if _, err := (Predicate{Op: OpEq, Field: "missing", Value: BoolValue(true)}).Eval(Context{}); err == nil {
		t.Fatalf("evaluating a predicate over an unbound field should error, never silently resolve false")
	}
}

func TestPredicate_UnrecognizedOpErrors(t *testing.T) {
	if _, err := (Predicate{Op: "wat"}).Eval(Context{}); err == nil {
		t.Fatalf("an unrecognized op must error — the whole point of a closed AST is refusing anything outside it")
	}
}
