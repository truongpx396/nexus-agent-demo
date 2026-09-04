package cost

import "testing"

func TestRegistry_LookupKnownAndUnknown(t *testing.T) {
	r := DefaultMeters()

	m, ok := r.Lookup(MeterOutput)
	if !ok {
		t.Fatal("MeterOutput not found in DefaultMeters()")
	}
	if !m.Reservable {
		t.Fatal("MeterOutput should be reservable (it's part of the token family)")
	}

	sandbox, ok := r.Lookup(MeterSandboxSeconds)
	if !ok {
		t.Fatal("MeterSandboxSeconds not found in DefaultMeters()")
	}
	if sandbox.Reservable {
		t.Fatal("MeterSandboxSeconds should NOT be reservable (task 4.2: registered but unemitted)")
	}

	if _, ok := r.Lookup(MeterID("does_not_exist")); ok {
		t.Fatal("unknown meter unexpectedly found")
	}
}

func TestRegistry_All(t *testing.T) {
	r := DefaultMeters()
	all := r.All()
	if len(all) != 7 {
		t.Fatalf("All() returned %d meters, want 7", len(all))
	}
	seen := map[MeterID]bool{}
	for _, m := range all {
		seen[m.ID] = true
	}
	for _, want := range []MeterID{MeterInputUncached, MeterInputCacheRead, MeterInputCacheWrite, MeterOutput, MeterEmbeddingTokens, MeterSandboxSeconds, MeterToolInvocations} {
		if !seen[want] {
			t.Errorf("All() missing meter %q", want)
		}
	}
}
