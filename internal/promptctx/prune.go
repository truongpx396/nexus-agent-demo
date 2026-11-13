package promptctx

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"unicode/utf8"

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
//
// A "tool" message holds one tool_result block per call the turn before it made,
// and every one of those calls must keep its answer: a hosted API rejects a
// tool_use with no tool_result. So pruning works on the blocks, each judged by
// its own size, and rewrites only a block's text. Its Kind, ToolUseID and IsError
// are left as they were, so the pruned view still passes
// provider.ValidateTranscript. A message is replaced in the view only when at
// least one of its blocks changed, and by a copy: the message's Blocks slice
// shares its backing array with the caller's transcript.
func Prune(transcript []provider.Message, policy PrunePolicy) ([]provider.Message, int) {
	if policy.KeepLast <= 0 && policy.SoftTrimAt <= 0 && policy.HardClearAt <= 0 {
		return transcript, 0
	}

	out := make([]provider.Message, len(transcript))
	copy(out, transcript)

	cutoff := len(out) - policy.KeepLast
	prunedCount := 0
	for i := range cutoff {
		if out[i].Role != "tool" {
			continue
		}
		blocks := slices.Clone(out[i].Blocks)
		changed := 0
		for j := range blocks {
			if blocks[j].Kind != provider.BlockToolResult && blocks[j].Kind != provider.BlockText {
				continue
			}
			if pruned, ok := pruneText(blocks[j].Text, policy); ok {
				blocks[j].Text = pruned
				changed++
			}
		}
		if changed > 0 {
			out[i] = provider.Message{Role: out[i].Role, Blocks: blocks}
			prunedCount += changed
		}
	}
	return out, prunedCount
}

// pruneText applies the two thresholds to one block's text: over HardClearAt it
// becomes the marker alone, over SoftTrimAt a preview and the marker. ok is false
// when the text is small enough to keep as it is.
func pruneText(text string, policy PrunePolicy) (string, bool) {
	switch {
	case policy.HardClearAt > 0 && len(text) > policy.HardClearAt:
		return pruneMarker(text), true
	case policy.SoftTrimAt > 0 && len(text) > policy.SoftTrimAt:
		return preview(text) + "\n" + pruneMarker(text), true
	}
	return text, false
}

// preview is the first previewBytes of text, cut back to a character boundary so
// a multi-byte character is never left half there.
func preview(text string) string {
	if len(text) <= previewBytes {
		return text
	}
	cut := previewBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// pruneMarker names what was removed (byte length) and a content digest a
// refetch could in principle resolve against — "hard clear to a refetchable
// reference" (task 7.10's own wording), even though no refetch tool ships
// in this phase.
func pruneMarker(original string) string {
	ref := sha256.Sum256([]byte(original))
	return fmt.Sprintf("[pruned: %d bytes, ref=%x]", len(original), ref[:8])
}
