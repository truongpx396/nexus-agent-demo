package promptctx

import (
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

func TestPrune_ZeroValuePolicyPrunesNothing(t *testing.T) {
	transcript := []provider.Message{provider.TextMessage("tool", strings.Repeat("x", 100000))}
	out, n := Prune(transcript, PrunePolicy{})
	if n != 0 {
		t.Errorf("prunedCount = %d, want 0 for a zero-value policy", n)
	}
	if out[0].PlainText() != transcript[0].PlainText() {
		t.Error("Prune modified the message under a zero-value policy")
	}
}

func TestPrune_NeverTouchesLastKeepLastMessages(t *testing.T) {
	transcript := make([]provider.Message, 10)
	for i := range transcript {
		transcript[i] = provider.TextMessage("tool", strings.Repeat("x", 5000))
	}
	out, _ := Prune(transcript, PrunePolicy{KeepLast: 3, SoftTrimAt: 10, HardClearAt: 20})
	for i := 7; i < 10; i++ {
		if out[i].PlainText() != transcript[i].PlainText() {
			t.Errorf("message %d within KeepLast was modified: got %q", i, out[i].PlainText())
		}
	}
	for i := range 7 {
		if out[i].PlainText() == transcript[i].PlainText() {
			t.Errorf("message %d beyond KeepLast was NOT pruned", i)
		}
	}
}

func TestPrune_NonToolMessagesAreNeverPruned(t *testing.T) {
	transcript := []provider.Message{
		provider.TextMessage("user", strings.Repeat("x", 100000)),
		provider.TextMessage("assistant", strings.Repeat("y", 100000)),
	}
	out, n := Prune(transcript, PrunePolicy{KeepLast: 0, SoftTrimAt: 10, HardClearAt: 20})
	if n != 0 {
		t.Errorf("prunedCount = %d, want 0 — only \"tool\" messages are ever pruned", n)
	}
	for i := range transcript {
		if out[i].PlainText() != transcript[i].PlainText() {
			t.Errorf("message %d was pruned despite role=%q", i, transcript[i].Role)
		}
	}
}

func TestPrune_HardClearDropsThePreviewSoftTrimKeeps(t *testing.T) {
	big := strings.Repeat("z", 1000)
	transcript := []provider.Message{provider.TextMessage("tool", big)}

	softOut, _ := Prune(transcript, PrunePolicy{KeepLast: 0, SoftTrimAt: 10, HardClearAt: 100000})
	if !strings.Contains(softOut[0].PlainText(), "pruned:") || !strings.HasPrefix(softOut[0].PlainText(), "zzz") {
		t.Errorf("soft trim output = %q, want a kept preview plus a pruned marker", softOut[0].PlainText())
	}

	hardOut, _ := Prune(transcript, PrunePolicy{KeepLast: 0, SoftTrimAt: 10, HardClearAt: 100})
	if strings.HasPrefix(hardOut[0].PlainText(), "zzz") {
		t.Errorf("hard clear output = %q, want no preview at all", hardOut[0].PlainText())
	}
	if !strings.Contains(hardOut[0].PlainText(), "pruned:") {
		t.Errorf("hard clear output = %q, want a pruned marker", hardOut[0].PlainText())
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
	for trial := range 300 {
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
			transcript[i] = provider.TextMessage(role, strings.Repeat("a", rng.IntN(20000)))
		}
		original := make([]provider.Message, len(transcript))
		copy(original, transcript)

		policy := PrunePolicy{KeepLast: rng.IntN(10), SoftTrimAt: rng.IntN(8000), HardClearAt: rng.IntN(20000)}
		out, prunedCount := Prune(transcript, policy)

		for i := range transcript {
			if !reflect.DeepEqual(transcript[i], original[i]) {
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

// toolTurn is an assistant turn calling n tools and the tool message answering
// them, each result of the given size: the shape a hosted API requires to stay
// paired.
func toolTurn(ids []string, size int) []provider.Message {
	var calls, results []provider.ContentBlock
	for _, id := range ids {
		calls = append(calls, provider.ContentBlock{Kind: provider.BlockToolUse, ToolUseID: id, ToolName: "platform/echo@v1", Input: []byte(`{}`)})
		results = append(results, provider.ContentBlock{Kind: provider.BlockToolResult, ToolUseID: id, Text: strings.Repeat("r", size), IsError: id == "tu2"})
	}
	return []provider.Message{{Role: "assistant", Blocks: calls}, {Role: "tool", Blocks: results}}
}

// deepCopy copies the messages and every block slice, so a later write through a
// shared backing array shows up as a difference.
func deepCopy(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		out[i] = provider.Message{Role: m.Role, Blocks: slices.Clone(m.Blocks)}
	}
	return out
}

// The finding: replacing a tool message with one text block dropped every
// tool_use_id, so the request carried calls with no results and a hosted API
// refused it.
func TestPrune_KeepsEveryToolResultAndItsCallId(t *testing.T) {
	transcript := []provider.Message{provider.TextMessage("user", "go")}
	transcript = append(transcript, toolTurn([]string{"tu1", "tu2"}, 6000)...)
	transcript = append(transcript, provider.TextMessage("assistant", "done"), provider.TextMessage("user", "next"))
	before := deepCopy(transcript)

	view, n := Prune(transcript, PrunePolicy{KeepLast: 2, SoftTrimAt: 1000, HardClearAt: 100000})

	if err := provider.ValidateTranscript(view); err != nil {
		t.Fatalf("the pruned view is not a valid transcript: %v", err)
	}
	if n != 2 {
		t.Errorf("pruned %d blocks, want 2 (each result is judged on its own)", n)
	}
	results := view[2].Blocks
	if len(results) != 2 {
		t.Fatalf("tool message has %d blocks, want its 2 results", len(results))
	}
	for i, id := range []string{"tu1", "tu2"} {
		b := results[i]
		if b.Kind != provider.BlockToolResult || b.ToolUseID != id {
			t.Errorf("block %d = %s %q, want a tool_result for %q", i, b.Kind, b.ToolUseID, id)
		}
		if len(b.Text) >= 6000 || !strings.Contains(b.Text, "pruned:") {
			t.Errorf("block %d text was not shortened: %d bytes", i, len(b.Text))
		}
	}
	if !results[1].IsError {
		t.Error("IsError was lost from a pruned result")
	}
	if !reflect.DeepEqual(transcript, before) {
		t.Fatal("Prune wrote through the caller's transcript (a shared Blocks array)")
	}
}

func TestPrune_JudgesEachResultOnItsOwn(t *testing.T) {
	// Two results of 3,000 bytes: 6,000 joined is over the soft threshold, but
	// neither is.
	transcript := append(toolTurn([]string{"tu1", "tu2"}, 3000), provider.TextMessage("user", "next"))
	view, n := Prune(transcript, PrunePolicy{KeepLast: 1, SoftTrimAt: 4000, HardClearAt: 100000})
	if n != 0 {
		t.Fatalf("pruned %d blocks, want 0: no single result is over the threshold", n)
	}
	if !reflect.DeepEqual(view, transcript) {
		t.Fatal("an unchanged transcript was replaced in the view")
	}

	// One large result next to a small one: only the large one is shortened.
	mixed := []provider.Message{
		{Role: "assistant", Blocks: []provider.ContentBlock{
			{Kind: provider.BlockToolUse, ToolUseID: "a", ToolName: "t", Input: []byte(`{}`)},
			{Kind: provider.BlockToolUse, ToolUseID: "b", ToolName: "t", Input: []byte(`{}`)},
		}},
		{Role: "tool", Blocks: []provider.ContentBlock{
			{Kind: provider.BlockToolResult, ToolUseID: "a", Text: strings.Repeat("x", 9000)},
			{Kind: provider.BlockToolResult, ToolUseID: "b", Text: "small"},
		}},
		provider.TextMessage("user", "next"),
	}
	view, n = Prune(mixed, PrunePolicy{KeepLast: 1, SoftTrimAt: 1000, HardClearAt: 100000})
	if n != 1 || view[1].Blocks[1].Text != "small" || len(view[1].Blocks[0].Text) >= 9000 {
		t.Fatalf("pruned %d blocks; results = %q / %q", n, view[1].Blocks[0].Text[:20], view[1].Blocks[1].Text)
	}
	if err := provider.ValidateTranscript(view); err != nil {
		t.Fatal(err)
	}
}

func TestPrune_APreviewNeverEndsInHalfACharacter(t *testing.T) {
	// "é" is two bytes, so the 256-byte preview boundary falls inside one.
	text := strings.Repeat("é", 1000)
	transcript := []provider.Message{provider.ToolResultMessage("tu1", text, false), provider.TextMessage("user", "next")}
	view, _ := Prune(transcript, PrunePolicy{KeepLast: 1, SoftTrimAt: 100, HardClearAt: 100000})
	if got := view[0].Blocks[0].Text; !utf8.ValidString(got) {
		t.Fatalf("pruned text is not valid UTF-8: %q", got)
	}
}

// The existing property test copies only the outer slice, which would not notice
// a write through a message's Blocks; this one copies every block.
func TestPrune_NeverWritesThroughASharedBlocksArray(t *testing.T) {
	seed := uint64(20261113)
	for trial := range 200 {
		rng := rand.New(rand.NewPCG(seed, uint64(trial))) //nolint:gosec // a property-test generator, not a security decision
		var transcript []provider.Message
		for turn := range rng.IntN(8) + 1 {
			var ids []string
			for k := range rng.IntN(3) + 1 {
				ids = append(ids, "tu"+strconv.Itoa(turn)+"-"+strconv.Itoa(k))
			}
			transcript = append(transcript, toolTurn(ids, rng.IntN(20000))...)
		}
		transcript = append(transcript, provider.TextMessage("user", "next"))
		before := deepCopy(transcript)

		view, _ := Prune(transcript, PrunePolicy{KeepLast: rng.IntN(4), SoftTrimAt: rng.IntN(8000), HardClearAt: rng.IntN(20000)})

		if !reflect.DeepEqual(transcript, before) {
			t.Fatalf("trial %d: Prune wrote through its input", trial)
		}
		if err := provider.ValidateTranscript(view); err != nil {
			t.Fatalf("trial %d: the pruned view is not a valid transcript: %v", trial, err)
		}
	}
}
