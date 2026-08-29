package permissions

import (
	"reflect"
	"testing"
)

// TestNoWideningMethodExists is README task 3.7's own acceptance line:
// "assert no widening function exists on any exported surface." Reflection
// over *Autonomy's method set is what makes this a build-breaking assertion
// rather than a comment someone has to remember to re-check by hand — a
// future edit that adds e.g. a "Widen" or "SetLevel" method fails this test
// the moment it's added, without needing to know what the method does.
func TestNoWideningMethodExists(t *testing.T) {
	typ := reflect.TypeOf(&Autonomy{})
	allowed := map[string]bool{
		"Level":   true, // read-only
		"Tighten": true, // the ratchet's only mutator, and it refuses to widen (see below)
		"Resolve": true, // read-only: produces a LayerOutcome, never mutates
	}
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		if !allowed[m.Name] {
			t.Errorf("Autonomy exports method %q — every exported method must be in `allowed` and reviewed for whether it could widen autonomy", m.Name)
		}
	}
}

func TestTighten(t *testing.T) {
	a := Pin(AutonomyAutonomous)
	if err := a.Tighten(AutonomySupervised); err != nil {
		t.Fatalf("Tighten(Supervised) from Autonomous: unexpected error %v", err)
	}
	if a.Level() != AutonomySupervised {
		t.Fatalf("Level() = %v, want Supervised", a.Level())
	}
	if err := a.Tighten(AutonomySupervised); err != nil {
		t.Fatalf("Tighten to the same level: unexpected error %v", err)
	}
	if err := a.Tighten(AutonomyAutonomous); err == nil {
		t.Fatal("Tighten(Autonomous) from Supervised: want an error (widening is refused)")
	}
	if a.Level() != AutonomySupervised {
		t.Fatalf("Level() after a refused widen = %v, want unchanged Supervised", a.Level())
	}
}

func TestAutonomyResolve(t *testing.T) {
	cases := []struct {
		level    AutonomyLevel
		effect   EffectClass
		decision Decision
	}{
		{AutonomyReadOnly, EffectClassReadOnly, Defer},
		{AutonomyReadOnly, EffectClassMutating, Deny},
		{AutonomyReadOnly, EffectClassExternal, Deny},
		{AutonomySupervised, EffectClassReadOnly, Defer},
		{AutonomySupervised, EffectClassMutating, Ask},
		{AutonomySupervised, EffectClassExternal, Ask},
		{AutonomyAutonomous, EffectClassReadOnly, Defer},
		{AutonomyAutonomous, EffectClassMutating, Defer},
		{AutonomyAutonomous, EffectClassExternal, Defer},
	}
	for _, tc := range cases {
		a := Pin(tc.level)
		got := a.Resolve(tc.effect)
		if got.Decision != tc.decision {
			t.Errorf("Pin(%v).Resolve(%v) = %v, want %v", tc.level, tc.effect, got.Decision, tc.decision)
		}
	}
}
