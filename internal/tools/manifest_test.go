package tools

import "testing"

func buildTestRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.DeclareNamespace("platform", "owner"); err != nil {
		t.Fatalf("DeclareNamespace: %v", err)
	}
	clean := newFakeTool("platform", "read_file", EffectClassReadOnly)
	pending := newFakeTool("platform", "unreviewed", EffectClassReadOnly)
	for _, tool := range []*fakeTool{clean, pending} {
		if err := reg.Register(tool); err != nil {
			t.Fatalf("Register(%v): %v", tool.ID(), err)
		}
	}
	if err := reg.SetAdmissionStatus(clean.ID(), AdmissionClean); err != nil {
		t.Fatalf("SetAdmissionStatus: %v", err)
	}
	// pending is left at AdmissionPending on purpose.
	return reg
}

func TestBuildManifest_OnlyIncludesCleanTools(t *testing.T) {
	reg := buildTestRegistry(t)
	m := BuildManifest(reg)
	if len(m.Entries) != 1 {
		t.Fatalf("Manifest has %d entries, want 1 (only the clean tool)", len(m.Entries))
	}
	if m.Entries[0].ID.Name != "read_file" {
		t.Fatalf("Manifest entry = %v, want read_file", m.Entries[0].ID)
	}
}

func TestManifest_ResolveOnlyFindsPinnedTools(t *testing.T) {
	reg := buildTestRegistry(t)
	m := BuildManifest(reg)

	if _, ok := m.Resolve(ToolRef{Namespace: "platform", Name: "read_file", Version: "v1"}); !ok {
		t.Fatal("Resolve(read_file) = not found, want found")
	}
	if _, ok := m.Resolve(ToolRef{Namespace: "platform", Name: "unreviewed", Version: "v1"}); ok {
		t.Fatal("Resolve(unreviewed) = found, want not found (it was never admitted clean)")
	}
}

func TestBuildManifest_DigestIsDeterministic(t *testing.T) {
	reg := buildTestRegistry(t)
	a := BuildManifest(reg)
	b := BuildManifest(reg)
	if string(a.Digest) != string(b.Digest) {
		t.Fatal("BuildManifest produced two different digests for the same registry state")
	}
	if len(a.Digest) == 0 {
		t.Fatal("Digest is empty")
	}
}

func TestBuildManifest_DigestChangesWithDescriptor(t *testing.T) {
	reg := NewRegistry()
	if err := reg.DeclareNamespace("platform", "owner"); err != nil {
		t.Fatalf("DeclareNamespace: %v", err)
	}
	tool := newFakeTool("platform", "read_file", EffectClassReadOnly)
	if err := reg.Register(tool); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.SetAdmissionStatus(tool.ID(), AdmissionClean); err != nil {
		t.Fatalf("SetAdmissionStatus: %v", err)
	}
	before := BuildManifest(reg)

	tool.desc.Description = "a different description"
	after := BuildManifest(reg)

	if string(before.Digest) == string(after.Digest) {
		t.Fatal("Digest did not change after the descriptor changed")
	}
}
