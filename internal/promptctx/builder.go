// Package promptctx builds the two-zone prompt constitution Principle III
// requires: a byte-stable prefix (stable system prompt + sorted resident
// tool catalog + the transcript committed so far) followed by a volatile
// tail rebuilt each turn. Cache stability is architecture, not a late
// optimization — this package is where that discipline is enforced, not left
// to convention at each call site.
package promptctx

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// Build assembles the prompt one turn is sent with, plus the prefix bytes
// that turn's call is measured against (README task 2.6). Phase 2 has no
// live pruning or structured compaction yet (Phase 7's
// prune.go/condense.go); until then the "volatile tail" is empty and every
// committed message stays in the prefix — the zone those later stages
// extend, not replace. transcript is caller-owned plaintext (kernel/loop.go
// keeps it in memory as it produces each message; promptctx never decrypts
// internal/store's sealed Payload itself).
func Build(system string, catalog []provider.ToolSchema, transcript []provider.Message) (provider.Prompt, []byte) {
	prefix := PrefixBytes(system, catalog, transcript)
	return provider.Prompt{
		System:   system,
		Messages: append([]provider.Message(nil), transcript...),
	}, prefix
}

// PrefixBytes returns the canonical byte encoding of the stable prefix.
// Encoding order is fixed and explicit — catalog entries sorted by name,
// transcript in append order, never map iteration — so identical inputs
// always produce identical bytes (the same discipline internal/harness.Digest
// applies, and for the same reason: cache identity must never depend on
// registration or iteration order).
func PrefixBytes(system string, catalog []provider.ToolSchema, transcript []provider.Message) []byte {
	sorted := append([]provider.ToolSchema(nil), catalog...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "system=%s\n", system)
	buf.WriteString("catalog:\n")
	for _, t := range sorted {
		fmt.Fprintf(&buf, "- %s: %s\n", t.Name, t.Description)
	}
	buf.WriteString("transcript:\n")
	for _, m := range transcript {
		fmt.Fprintf(&buf, "%s: %s\n", m.Role, m.Text)
	}
	return buf.Bytes()
}
