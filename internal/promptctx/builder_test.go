package promptctx

import (
	"bytes"
	"fmt"
	"math"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// TestPrefixBytesEquality is README task 2.7's "prefix byte-equality test
// across N turns": as the transcript grows by strict append (the only way
// it ever changes, per Principle II), the prefix at turn N+1 must be a
// byte-for-byte extension of the prefix at turn N — never a re-derivation
// that happens to match.
func TestPrefixBytesEquality(t *testing.T) {
	catalog := []provider.ToolSchema{{Name: "b_tool", Description: "second"}, {Name: "a_tool", Description: "first"}}
	system := "You are a helpful agent."

	var transcript []provider.Message
	var prev []byte
	for turn := 1; turn <= 5; turn++ {
		transcript = append(transcript,
			provider.Message{Role: "user", Text: fmt.Sprintf("turn %d input", turn)},
			provider.Message{Role: "assistant", Text: fmt.Sprintf("turn %d output", turn)},
		)
		cur := PrefixBytes(system, catalog, transcript)
		if prev != nil && !bytes.HasPrefix(cur, prev) {
			t.Fatalf("turn %d: prefix is not a byte-extension of turn %d's prefix\nprev=%q\ncur=%q", turn, turn-1, prev, cur)
		}
		if len(prev) >= len(cur) {
			t.Fatalf("turn %d: prefix did not grow", turn)
		}
		prev = cur
	}
}

func TestPrefixBytesCatalogOrderIndependent(t *testing.T) {
	system := "sys"
	a := PrefixBytes(system, []provider.ToolSchema{{Name: "a"}, {Name: "b"}}, nil)
	b := PrefixBytes(system, []provider.ToolSchema{{Name: "b"}, {Name: "a"}}, nil)
	if !bytes.Equal(a, b) {
		t.Fatal("catalog registration order must not affect prefix bytes")
	}
}

func TestCacheReadRate(t *testing.T) {
	usages := []provider.Usage{
		{InputUncached: 100},
		{InputCacheRead: 900, InputUncached: 100},
	}
	got := CacheReadRate(usages)
	want := 900.0 / 1100.0
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestCacheReadRateNoUsage(t *testing.T) {
	if got := CacheReadRate(nil); got != 0 {
		t.Fatalf("got %v want 0", got)
	}
}
