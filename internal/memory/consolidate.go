package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// Consolidate replaces tenantID's memory file `name` with a condensed
// version — an ordered, metered, degrade-capable stage (README task 7.2):
// the durable write precedes the compaction that would discard its source.
// reserve reports whether a metered model call is affordable right now
// (kernel/cmd wires this to cost.BudgetGate.Reserve with
// cost.PurposeCompaction — "off the paying loop" means cheaper, never
// unmetered); when reserve() is false, or condenser itself errors, this
// falls back to the local no-model ExtractivePass rather than skipping
// consolidation altogether ("degrade-capable").
//
// Ordering: the consolidated content is written to a temp file and fsynced
// — fully durable on disk — BEFORE os.Rename atomically replaces the
// source. A crash at any point before the rename leaves the original source
// completely untouched; a crash after it leaves the new content fully in
// place. There is no window in which the source is gone but the
// consolidated replacement isn't durable yet.
func (s *Store) Consolidate(tenantID uuid.UUID, name string, reserve func() bool, condenser func(text string) (string, error)) error {
	sourcePath := filepath.Join(s.tenantDir(tenantID), name)
	original, err := os.ReadFile(sourcePath) //nolint:gosec // sourcePath is built from this tenant's own memory dir, never request input
	if err != nil {
		return fmt.Errorf("memory: read source %s: %w", sourcePath, err)
	}

	consolidated := ""
	degraded := true
	if reserve() {
		summary, cerr := condenser(string(original))
		if cerr == nil {
			consolidated = summary
			degraded = false
		}
	}
	if degraded {
		consolidated = ExtractivePass(string(original))
	}

	tmpPath := sourcePath + ".consolidating"
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // tmpPath is derived from this tenant's own memory dir, never external input

	if err != nil {
		return fmt.Errorf("memory: open temp file: %w", err)
	}
	if _, werr := f.WriteString(consolidated); werr != nil {
		_ = f.Close()
		return fmt.Errorf("memory: write temp file: %w", werr)
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		return fmt.Errorf("memory: fsync temp file: %w", serr)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("memory: close temp file: %w", cerr)
	}

	if err := os.Rename(tmpPath, sourcePath); err != nil {
		return fmt.Errorf("memory: rename consolidated file over source: %w", err)
	}
	return nil
}

// ExtractivePass is the no-model degrade path: deterministic, never calls a
// model. Keeps the first and last non-empty lines verbatim and collapses
// everything between them to a count — enough to preserve the shape of a
// memory file's start/end without an LLM call.
func ExtractivePass(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	switch len(nonEmpty) {
	case 0:
		return ""
	case 1:
		return nonEmpty[0]
	case 2:
		return nonEmpty[0] + "\n" + nonEmpty[1]
	default:
		return fmt.Sprintf("%s\n[extractive: %d lines omitted]\n%s", nonEmpty[0], len(nonEmpty)-2, nonEmpty[len(nonEmpty)-1])
	}
}
