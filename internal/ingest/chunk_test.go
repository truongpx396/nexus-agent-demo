package ingest

import (
	"strings"
	"testing"
)

func TestSplitText_ParagraphBoundaries(t *testing.T) {
	text := "First paragraph.\n\nSecond paragraph."
	chunks := SplitText(text)
	if len(chunks) != 1 {
		t.Fatalf("expected short paragraphs to pack into one chunk, got %d: %v", len(chunks), chunks)
	}
	if !strings.Contains(chunks[0], "First paragraph.") || !strings.Contains(chunks[0], "Second paragraph.") {
		t.Errorf("expected both paragraphs in the packed chunk, got %q", chunks[0])
	}
}

func TestSplitText_LongParagraphIsHardSplit(t *testing.T) {
	long := strings.Repeat("a", defaultChunkChars*3)
	chunks := SplitText(long)
	if len(chunks) < 2 {
		t.Fatalf("expected an over-long paragraph to be split into multiple chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if runeLen(c) > defaultChunkChars {
			t.Errorf("chunk exceeds defaultChunkChars: len=%d", runeLen(c))
		}
	}
}

func TestSplitText_Deterministic(t *testing.T) {
	text := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200)
	a := SplitText(text)
	b := SplitText(text)
	if len(a) != len(b) {
		t.Fatalf("non-deterministic chunk count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic chunk %d content", i)
		}
	}
}

func TestSplitText_EmptyInput(t *testing.T) {
	if chunks := SplitText(""); len(chunks) != 0 {
		t.Errorf("expected no chunks for empty text, got %d", len(chunks))
	}
	if chunks := SplitText("   \n\n  "); len(chunks) != 0 {
		t.Errorf("expected no chunks for whitespace-only text, got %d", len(chunks))
	}
}

func TestSplitText_ManyParagraphsPackIntoMultipleChunks(t *testing.T) {
	var paras []string
	for i := 0; i < 50; i++ {
		paras = append(paras, strings.Repeat("word ", 20))
	}
	text := strings.Join(paras, "\n\n")
	chunks := SplitText(text)
	if len(chunks) < 2 {
		t.Fatalf("expected many paragraphs to pack into multiple chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if runeLen(c) > defaultChunkChars {
			t.Errorf("packed chunk exceeds defaultChunkChars: len=%d", runeLen(c))
		}
	}
}
