package tools

import "testing"

func TestParseToolRef(t *testing.T) {
	got, err := ParseToolRef("platform/shell@v1")
	if err != nil {
		t.Fatalf("ParseToolRef error = %v", err)
	}
	want := ToolRef{Namespace: "platform", Name: "shell", Version: "v1"}
	if got != want {
		t.Fatalf("ParseToolRef = %+v, want %+v", got, want)
	}
	if got.String() != "platform/shell@v1" {
		t.Fatalf("String() = %q, want %q", got.String(), "platform/shell@v1")
	}
}

func TestParseToolRef_Malformed(t *testing.T) {
	for _, s := range []string{"", "no-at-sign", "platform/shell", "@v1", "platform/shell@", "platform//shell@v1"} {
		if _, err := ParseToolRef(s); err == nil {
			t.Errorf("ParseToolRef(%q) = nil error, want an error", s)
		}
	}
}

func TestRegistry_NamespaceCollisionRefusedAtAdmission(t *testing.T) {
	reg := NewRegistry()
	if err := reg.DeclareNamespace("platform", "owner-a"); err != nil {
		t.Fatalf("first DeclareNamespace: %v", err)
	}
	if err := reg.DeclareNamespace("platform", "owner-b"); err == nil {
		t.Fatal("DeclareNamespace with a different owner = nil error, want a refusal")
	}
	// The same owner re-declaring is idempotent, not a collision.
	if err := reg.DeclareNamespace("platform", "owner-a"); err != nil {
		t.Fatalf("re-declaring with the same owner: unexpected error %v", err)
	}
}

func TestRegistry_RegisterRequiresDeclaredNamespace(t *testing.T) {
	reg := NewRegistry()
	tool := newFakeTool("acme", "custom", EffectClassReadOnly)
	if err := reg.Register(tool); err == nil {
		t.Fatal("Register into an undeclared namespace = nil error, want a refusal")
	}
}

func TestRegistry_DuplicateRegistrationRefused(t *testing.T) {
	reg := NewRegistry()
	if err := reg.DeclareNamespace("platform", "owner"); err != nil {
		t.Fatalf("DeclareNamespace: %v", err)
	}
	tool := newFakeTool("platform", "shell", EffectClassMutating)
	if err := reg.Register(tool); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := reg.Register(tool); err == nil {
		t.Fatal("second Register of the same ref = nil error, want a refusal")
	}
}

func TestRegistry_AllIsDeterministicallyOrdered(t *testing.T) {
	reg := NewRegistry()
	for _, ns := range []string{"zeta", "alpha", "mid"} {
		if err := reg.DeclareNamespace(ns, "owner"); err != nil {
			t.Fatalf("DeclareNamespace(%s): %v", ns, err)
		}
	}
	_ = reg.Register(newFakeTool("zeta", "z1", EffectClassReadOnly))
	_ = reg.Register(newFakeTool("alpha", "a1", EffectClassReadOnly))
	_ = reg.Register(newFakeTool("mid", "m1", EffectClassReadOnly))

	first := reg.All()
	second := reg.All()
	if len(first) != 3 {
		t.Fatalf("All() returned %d tools, want 3", len(first))
	}
	for i := range first {
		if first[i].ID() != second[i].ID() {
			t.Fatalf("All() is not deterministic across calls: %v vs %v", first[i].ID(), second[i].ID())
		}
	}
	if first[0].ID().Namespace != "alpha" {
		t.Fatalf("All()[0].Namespace = %q, want %q (namespace-sorted)", first[0].ID().Namespace, "alpha")
	}
}
