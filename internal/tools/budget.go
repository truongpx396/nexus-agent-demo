package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// DerivedArtifactRecorder tracks a plaintext-derived file living outside
// the encrypted event log — today, only a BudgetResult spill — so
// internal/crypto/shred.go's erasure and reconciliation (README task 5.4)
// have something to find and hard-delete. Declared as a function type here,
// not imported from a store/DB package, so internal/tools stays free of a
// direct DB dependency (its existing, zero-DB design); cmd/nexusd wires the
// real store-backed recorder in. Nil is valid and simply means spills go
// untracked (every pre-Phase-5 caller, including every test in this file).
type DerivedArtifactRecorder func(ctx context.Context, tenantID, sessionID uuid.UUID, kind, path string) error

// approxCharsPerToken is a rough, documented estimate — this demo has no
// tokenizer of its own in internal/tools, and an approximation is the
// pragmatic choice budget-shaping a preview needs (the exact count a real
// deployment bills on comes from Provider.Usage, a different concern).
const approxCharsPerToken = 4

// maxResultTokens is task 3.13's "~25k tokens" cap.
const maxResultTokens = 25000
const maxResultBytes = maxResultTokens * approxCharsPerToken

// BlobStore is the "local blob dir" the infrastructure-collapse table
// (README.md §2) names in place of S3 — a directory an oversized tool
// result spills to so the transcript only ever carries a bounded preview.
type BlobStore struct {
	Dir string
}

func sanitizeForFilename(s string) string {
	r := strings.NewReplacer("/", "_", "@", "_", ":", "_")
	return r.Replace(s)
}

// Spill writes content to a new file under Dir and returns its path.
func (b BlobStore) Spill(sessionID uuid.UUID, toolID string, content []byte) (string, error) {
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return "", fmt.Errorf("create blob dir: %w", err)
	}
	name := fmt.Sprintf("%s-%s-%s.blob", sessionID, sanitizeForFilename(toolID), uuid.New())
	path := filepath.Join(b.Dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return "", fmt.Errorf("write blob %s: %w", path, err)
	}
	return path, nil
}

// budgetedResult is the JSON shape a caller sees once a result has been
// spilled — Preview never claims more than it is: the banner is baked into
// the text itself so it survives being copied out of context.
type budgetedResult struct {
	Preview        string `json:"preview"`
	FullResultPath string `json:"full_result_path"`
	Truncated      bool   `json:"truncated"`
}

// BudgetResult is pipeline step 15 (README task 3.13): output at or under
// the cap passes through unchanged; anything larger is spilled to blobs and
// replaced with a preview plus the "do not infer success from the preview"
// banner the task names verbatim. recorder, if non-nil, tracks the spilled
// file as a derived artifact (task 5.4) — a recorder failure is logged into
// the returned error only when nothing else already fires (it never
// unwinds a spill that already succeeded and is already on disk).
func BudgetResult(ctx context.Context, blobs BlobStore, tenantID, sessionID uuid.UUID, toolID string, output json.RawMessage, recorder DerivedArtifactRecorder) (json.RawMessage, error) {
	if len(output) <= maxResultBytes {
		return output, nil
	}
	path, err := blobs.Spill(sessionID, toolID, output)
	if err != nil {
		return nil, fmt.Errorf("tools: spill oversized result: %w", err)
	}
	if recorder != nil {
		if err := recorder(ctx, tenantID, sessionID, "blob", path); err != nil {
			return nil, fmt.Errorf("tools: record derived artifact %s: %w", path, err)
		}
	}
	preview := string(output[:maxResultBytes])
	preview += fmt.Sprintf("\n\n[preview truncated at ~%d tokens; full result spilled to %s — do not infer success from the preview]", maxResultTokens, path)

	out, err := json.Marshal(budgetedResult{Preview: preview, FullResultPath: path, Truncated: true})
	if err != nil {
		return nil, fmt.Errorf("tools: marshal budgeted result: %w", err)
	}
	return out, nil
}
