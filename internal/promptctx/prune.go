package promptctx

import (
	"crypto/sha256"
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// PrunePolicy configures live pruning (README task 7.10): three ordered
// stages applied only to "tool" role messages (tool_result bodies — the
// largest, most re-fetchable content; user/assistant messages are always
// kept verbatim since they're the actual conversation, not a cache of
// something else) that are more than KeepLast messages from the end of the
// transcript. A candidate over SoftTrimAt bytes gets a truncated preview
// plus a refetchable-reference marker (the outlier guard / soft trim
// stages); one over HardClearAt gets only the marker (hard clear). Byte
// length, not a token count, is the declared, measured proxy this codebase
// uses everywhere it lacks a tokenizer (condense.go's ShouldCondense does
// the same).
type PrunePolicy struct {
	KeepLast    int
	SoftTrimAt  int
	HardClearAt int
}

// DefaultPrunePolicy is a conservative default for a caller that wants
// pruning without hand-tuning thresholds.
func DefaultPrunePolicy() PrunePolicy {
	return PrunePolicy{KeepLast: 20, SoftTrimAt: 4096, HardClearAt: 16384}
}

// previewBytes bounds a soft-trimmed message's kept preview.
const previewBytes = 256

// Prune returns a pruned VIEW of transcript for Build to use — it never
// mutates transcript or any of its elements (task 7.10: "never mutates a
// logged event"): kernel/loop.go's own st.Transcript, the in-memory
// projection every other stage of a turn still reads, is passed in here and
// left completely untouched; only the returned copy is pruned. A zero-value
// policy ({0,0,0}) prunes nothing — the pre-Phase-7 behavior a caller that
// never sets Kernel.PrunePolicy still gets.
func Prune(transcript []provider.Message, policy PrunePolicy) ([]provider.Message, int) {
	if policy.KeepLast <= 0 && policy.SoftTrimAt <= 0 && policy.HardClearAt <= 0 {
		return transcript, 0
	}

	out := make([]provider.Message, len(transcript))
	copy(out, transcript)

	cutoff := len(out) - policy.KeepLast
	prunedCount := 0
	for i := 0; i < cutoff; i++ {
		m := out[i]
		if m.Role != "tool" {
			continue
		}
		switch {
		case policy.HardClearAt > 0 && len(m.Text) > policy.HardClearAt:
			out[i] = provider.Message{Role: m.Role, Text: pruneMarker(m.Text)}
			prunedCount++
		case policy.SoftTrimAt > 0 && len(m.Text) > policy.SoftTrimAt:
			preview := m.Text
			if len(preview) > previewBytes {
				preview = preview[:previewBytes]
			}
			out[i] = provider.Message{Role: m.Role, Text: preview + "\n" + pruneMarker(m.Text)}
			prunedCount++
		}
	}
	return out, prunedCount
}

// pruneMarker names what was removed (byte length) and a content digest a
// refetch could in principle resolve against — "hard clear to a refetchable
// reference" (task 7.10's own wording), even though no refetch tool ships
// in this phase.
func pruneMarker(original string) string {
	ref := sha256.Sum256([]byte(original))
	return fmt.Sprintf("[pruned: %d bytes, ref=%x]", len(original), ref[:8])
}
