package promptctx

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

func TestPrune_ZeroValuePolicyPrunesNothing(t *testing.T) {
	transcript := []provider.Message{{Role: "tool", Text: strings.Repeat("x", 100000)}}
	out, n := Prune(transcript, PrunePolicy{})
	if n != 0 {
		t.Errorf("prunedCount = %d, want 0 for a zero-value policy", n)
	}
	if out[0].Text != transcript[0].Text {
		t.Error("Prune modified the message under a zero-value policy")
	}
}

func TestPrune_NeverTouchesLastKeepLastMessages(t *testing.T) {
	transcript := make([]provider.Message, 10)
	for i := range transcript {
		transcript[i] = provider.Message{Role: "tool", Text: strings.Repeat("x", 5000)}
	}
	out, _ := Prune(transcript, PrunePolicy{KeepLast: 3, SoftTrimAt: 10, HardClearAt: 20})
	for i := 7; i < 10; i++ {
		if out[i].Text != transcript[i].Text {
			t.Errorf("message %d within KeepLast was modified: got %q", i, out[i].Text)
		}
	}
	for i := 0; i < 7; i++ {
		if out[i].Text == transcript[i].Text {
			t.Errorf("message %d beyond KeepLast was NOT pruned", i)
		}
	}
}

func TestPrune_NonToolMessagesAreNeverPruned(t *testing.T) {
	transcript := []provider.Message{
		{Role: "user", Text: strings.Repeat("x", 100000)},
		{Role: "assistant", Text: strings.Repeat("y", 100000)},
	}
	out, n := Prune(transcript, PrunePolicy{KeepLast: 0, SoftTrimAt: 10, HardClearAt: 20})
	if n != 0 {
		t.Errorf("prunedCount = %d, want 0 — only \"tool\" messages are ever pruned", n)
	}
	for i := range transcript {
		if out[i].Text != transcript[i].Text {
			t.Errorf("message %d was pruned despite role=%q", i, transcript[i].Role)
		}
	}
}

func TestPrune_HardClearDropsThePreviewSoftTrimKeeps(t *testing.T) {
	big := strings.Repeat("z", 1000)
	transcript := []provider.Message{{Role: "tool", Text: big}}

	softOut, _ := Prune(transcript, PrunePolicy{KeepLast: 0, SoftTrimAt: 10, HardClearAt: 100000})
	if !strings.Contains(softOut[0].Text, "pruned:") || !strings.HasPrefix(softOut[0].Text, "zzz") {
		t.Errorf("soft trim output = %q, want a kept preview plus a pruned marker", softOut[0].Text)
	}

	hardOut, _ := Prune(transcript, PrunePolicy{KeepLast: 0, SoftTrimAt: 10, HardClearAt: 100})
	if strings.HasPrefix(hardOut[0].Text, "zzz") {
		t.Errorf("hard clear output = %q, want no preview at all", hardOut[0].Text)
	}
	if !strings.Contains(hardOut[0].Text, "pruned:") {
		t.Errorf("hard clear output = %q, want a pruned marker", hardOut[0].Text)
	}
}

// TestPrune_NeverMutatesInput is a property test (math/rand/v2, never the
// forbidigo-banned math/rand — this generator backs no security decision):
// over many randomly generated transcripts and policies, Prune's output
// never shares backing memory with the input and the input is byte-for-byte
// unchanged after the call — task 7.10's "never mutates a logged event"
// made a checked invariant rather than a hoped-for property of the
// implementation above.
func TestPrune_NeverMutatesInput(t *testing.T) {
	seed := uint64(20260830)
	for trial := 0; trial < 300; trial++ {
		rng := rand.New(rand.NewPCG(seed, uint64(trial))) //nolint:gosec // a property-test generator, not a security decision
		n := rng.IntN(30)
		transcript := make([]provider.Message, n)
		for i := range transcript {
			role := "tool"
			switch rng.IntN(3) {
			case 1:
				role = "user"
			case 2:
				role = "assistant"
			}
			transcript[i] = provider.Message{Role: role, Text: strings.Repeat("a", rng.IntN(20000))}
		}
		original := make([]provider.Message, len(transcript))
		copy(original, transcript)

		policy := PrunePolicy{KeepLast: rng.IntN(10), SoftTrimAt: rng.IntN(8000), HardClearAt: rng.IntN(20000)}
		out, prunedCount := Prune(transcript, policy)

		for i := range transcript {
			if transcript[i] != original[i] {
				t.Fatalf("trial %d: Prune mutated its input transcript at index %d", trial, i)
			}
		}
		if len(out) != len(transcript) {
			t.Fatalf("trial %d: Prune changed the message count: got %d, want %d", trial, len(out), len(transcript))
		}
		if prunedCount < 0 || prunedCount > len(out) {
			t.Fatalf("trial %d: prunedCount = %d out of range [0,%d]", trial, prunedCount, len(out))
		}
	}
}
