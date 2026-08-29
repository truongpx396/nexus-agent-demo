package kernel

import (
	"sort"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// SyntheticResult is what Hygiene reports for one orphaned tool_use. Hygiene
// itself stays pure (no crypto, no store dependency) so it's cheap to
// property-test (tests/property/paired_result_test.go); the caller
// (kernel/loop.go) has the SealFunc and store.Store needed to turn each of
// these into a real, sealed, appended EventToolResult.
type SyntheticResult struct {
	PairRef uuid.UUID // the tool_use's EventID
	ToolID  *string
	Reason  string
}

// Hygiene is the pre-call pass README task 2.4 names, run before every model
// call (kernel/loop.go):
//
//   - drop a tool_result whose pair_ref names no tool_use in history (an
//     orphan — e.g. left behind by a prior schema change or a bug elsewhere)
//   - drop a tool_result with no pair_ref at all
//   - drop a duplicate tool_result for a pair_ref already resolved, keeping
//     the first (defensive: the paired-result invariant is "exactly one",
//     never "at least one")
//   - report a SyntheticResult for every tool_use left with no result, so
//     the caller can backfill one before the next model call
//
// This is what proves Phase 2's "kill mid-turn, the log still shows a paired
// result for every tool_use" acceptance line (README §5, Phase 2) at the
// unit level: a history reconstructed from a session interrupted between
// appending a tool_use and its result comes out of Hygiene with that gap
// already reported. Automatic re-trigger on process restart is Phase 6's
// queue + checkpoint (README tasks 6.1–6.3); Hygiene is the mechanism they
// call, not the trigger.
func Hygiene(history []store.Event) (kept []store.Event, synthesize []SyntheticResult) {
	toolUses := make(map[uuid.UUID]store.Event, len(history))
	for _, e := range history {
		if e.Type == store.EventToolUse {
			toolUses[e.EventID] = e
		}
	}

	seenResultFor := make(map[uuid.UUID]bool, len(toolUses))
	kept = make([]store.Event, 0, len(history))
	for _, e := range history {
		if e.Type != store.EventToolResult {
			kept = append(kept, e)
			continue
		}
		if e.PairRef == nil {
			continue
		}
		if _, ok := toolUses[*e.PairRef]; !ok {
			continue
		}
		if seenResultFor[*e.PairRef] {
			continue
		}
		seenResultFor[*e.PairRef] = true
		kept = append(kept, e)
	}

	for id, tu := range toolUses {
		if seenResultFor[id] {
			continue
		}
		synthesize = append(synthesize, SyntheticResult{
			PairRef: id,
			ToolID:  tu.ToolID,
			Reason:  "interrupted_before_execution",
		})
	}
	// Map iteration order is randomized; sort so a given history always
	// produces the same synthesize order (map iteration is not a source of
	// non-determinism this package wants to leak to its callers or tests).
	sort.Slice(synthesize, func(i, j int) bool {
		return synthesize[i].PairRef.String() < synthesize[j].PairRef.String()
	})

	return kept, synthesize
}
