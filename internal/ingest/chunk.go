package ingest

import "strings"

// defaultChunkChars and defaultChunkOverlap are this demo's fixed chunking
// policy — task 12.1's "declared, stable chunk boundaries" only means
// something if the same document always splits the same way, so these are
// constants, not a caller-tunable knob that would make chunk_index
// non-reproducible across two ingests of the same file.
const (
	defaultChunkChars   = 1000
	defaultChunkOverlap = 100
)

// SplitText splits text into ordered, non-empty chunk strings: it first breaks on
// paragraph boundaries (blank lines) so a chunk boundary never falls
// mid-sentence when the source has any paragraph structure at all, then
// packs consecutive paragraphs into windows of at most defaultChunkChars
// runes. A single paragraph longer than the window is hard-split at rune
// boundaries with defaultChunkOverlap runes of overlap, so no window is
// ever unbounded and a search hit near a hard split still has surrounding
// context on both sides.
func SplitText(text string) []string {
	paragraphs := splitParagraphs(text)
	var chunks []string
	var current strings.Builder

	flush := func() {
		if s := strings.TrimSpace(current.String()); s != "" {
			chunks = append(chunks, s)
		}
		current.Reset()
	}

	for _, p := range paragraphs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if runeLen(p) > defaultChunkChars {
			flush()
			chunks = append(chunks, hardSplit(p)...)
			continue
		}
		if current.Len() > 0 && runeLen(current.String())+runeLen(p)+1 > defaultChunkChars {
			flush()
		}
		if current.Len() > 0 {
			current.WriteByte('\n')
		}
		current.WriteString(p)
	}
	flush()
	return chunks
}

func splitParagraphs(text string) []string {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Split(normalized, "\n\n")
}

func runeLen(s string) int { return len([]rune(s)) }

// hardSplit windows a single over-long paragraph into fixed-size,
// overlapping rune slices — the fallback for source text with no paragraph
// structure at all (e.g. one huge line), so Chunk never returns a window
// larger than defaultChunkChars regardless of input shape.
func hardSplit(s string) []string {
	runes := []rune(s)
	var out []string
	step := defaultChunkChars - defaultChunkOverlap
	for start := 0; start < len(runes); start += step {
		end := min(start+defaultChunkChars, len(runes))
		out = append(out, string(runes[start:end]))
		if end == len(runes) {
			break
		}
	}
	return out
}
