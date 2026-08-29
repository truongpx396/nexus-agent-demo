// Package property holds tests over *generated* event histories rather than
// hand-picked examples — the paired tool_use/tool_result invariant is total
// (constitution Principle II), so example tests can only ever show its
// presence, never its absence (README task 2.5).
package property

import (
	"math/rand/v2"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// genHistory builds one randomized event history: a mix of correctly paired
// tool_use/tool_result runs, orphan tool_uses (no result — the "killed
// mid-turn" case), orphan tool_results (pair_ref naming nothing in this
// history), duplicate results for an already-resolved pair_ref, a nil-pair_ref
// result, and unrelated content events as noise. rng is math/rand/v2 — a
// distinct package from the forbidigo-banned math/rand (.golangci.yml bans
// `^math/rand\.`, not `math/rand/v2`), and the correct choice here regardless:
// this generator backs no security decision, so the ban's rationale doesn't
// apply.
func genHistory(rng *rand.Rand, n int) (history []store.Event, wantPaired map[uuid.UUID]bool) {
	wantPaired = map[uuid.UUID]bool{}

	for i := 0; i < n; i++ {
		switch rng.IntN(6) {
		case 0, 1: // paired tool_use + tool_result
			tu := store.Event{EventID: uuid.New(), Type: store.EventToolUse}
			history = append(history, tu)
			wantPaired[tu.EventID] = true
			ref := tu.EventID
			history = append(history, store.Event{EventID: uuid.New(), Type: store.EventToolResult, PairRef: &ref})

		case 2: // orphan tool_use, no result at all — the "killed mid-turn" case
			tu := store.Event{EventID: uuid.New(), Type: store.EventToolUse}
			history = append(history, tu)
			wantPaired[tu.EventID] = true

		case 3: // orphan tool_result: pair_ref names nothing in this history
			ref := uuid.New()
			history = append(history, store.Event{EventID: uuid.New(), Type: store.EventToolResult, PairRef: &ref})

		case 4: // tool_use resolved, then a stray duplicate result for the same pair_ref
			tu := store.Event{EventID: uuid.New(), Type: store.EventToolUse}
			history = append(history, tu)
			wantPaired[tu.EventID] = true
			ref := tu.EventID
			history = append(history, store.Event{EventID: uuid.New(), Type: store.EventToolResult, PairRef: &ref})
			history = append(history, store.Event{EventID: uuid.New(), Type: store.EventToolResult, PairRef: &ref})

		default: // noise: an unrelated event, and a nil-pair_ref result
			history = append(history, store.Event{EventID: uuid.New(), Type: store.EventContent})
			if rng.IntN(2) == 0 {
				history = append(history, store.Event{EventID: uuid.New(), Type: store.EventToolResult, PairRef: nil})
			}
		}
	}
	return history, wantPaired
}

// TestHygienePairedResultInvariant is the property README task 2.5 names:
// over generated event histories, every tool_use ends up with exactly one
// tool_result (kept in the repaired history, or reported as a
// SyntheticResult) before the next model call — never zero, never more than
// one — and no result ever survives pointing at a tool_use absent from the
// repaired history.
func TestHygienePairedResultInvariant(t *testing.T) {
	seed := uint64(12345)
	for trial := 0; trial < 300; trial++ {
		rng := rand.New(rand.NewPCG(seed, uint64(trial))) //nolint:gosec // a property-test generator, not a security decision — see genHistory's doc comment
		n := rng.IntN(30)
		history, wantPaired := genHistory(rng, n)

		kept, synth := kernel.Hygiene(history)

		keptToolUses := map[uuid.UUID]bool{}
		resultCountFor := map[uuid.UUID]int{}
		for _, e := range kept {
			switch e.Type { //nolint:exhaustive // only tool_use/tool_result are structurally relevant to the paired-result invariant; every other event type is deliberately-ignored noise (genHistory's default case)
			case store.EventToolUse:
				keptToolUses[e.EventID] = true
			case store.EventToolResult:
				if e.PairRef == nil {
					t.Fatalf("trial %d: a nil-pair_ref tool_result survived Hygiene", trial)
				}
				if !keptToolUses[*e.PairRef] {
					t.Fatalf("trial %d: a tool_result paired to a tool_use not in the repaired history survived Hygiene", trial)
				}
				resultCountFor[*e.PairRef]++
			}
		}
		for _, s := range synth {
			if !keptToolUses[s.PairRef] {
				t.Fatalf("trial %d: a SyntheticResult pairs to a tool_use not in the repaired history", trial)
			}
			resultCountFor[s.PairRef]++
		}

		for id := range wantPaired {
			if !keptToolUses[id] {
				// The generator never drops a tool_use itself — Hygiene must not either.
				t.Fatalf("trial %d: tool_use %s vanished from the repaired history", trial, id)
			}
			if resultCountFor[id] != 1 {
				t.Fatalf("trial %d: tool_use %s has %d results after Hygiene, want exactly 1", trial, id, resultCountFor[id])
			}
		}
		for id, count := range resultCountFor {
			if !keptToolUses[id] {
				t.Fatalf("trial %d: a result resolved a tool_use (%s) the generator never produced", trial, id)
			}
			if count != 1 {
				t.Fatalf("trial %d: tool_use %s has %d results, want exactly 1", trial, id, count)
			}
		}
	}
}
