package tools

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestBudgetResult_UnderCapPassesThrough(t *testing.T) {
	blobs := BlobStore{Dir: t.TempDir()}
	small := json.RawMessage(`{"ok":true}`)
	out, err := BudgetResult(blobs, uuid.New(), "x/y@v1", small)
	if err != nil {
		t.Fatalf("BudgetResult: %v", err)
	}
	if string(out) != string(small) {
		t.Fatalf("BudgetResult modified an under-cap result: got %s", out)
	}
}

func TestBudgetResult_OverCapSpillsAndPreviewsWithBanner(t *testing.T) {
	dir := t.TempDir()
	blobs := BlobStore{Dir: dir}
	big := json.RawMessage(`"` + strings.Repeat("y", maxResultBytes+500) + `"`)
	sessionID := uuid.New()

	out, err := BudgetResult(blobs, sessionID, "platform/shell@v1", big)
	if err != nil {
		t.Fatalf("BudgetResult: %v", err)
	}
	var decoded budgetedResult
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("output is not a budgetedResult: %v", err)
	}
	if !decoded.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if !strings.Contains(decoded.Preview, "do not infer success from the preview") {
		t.Fatalf("Preview missing the required banner: %s", decoded.Preview)
	}
	content, err := os.ReadFile(decoded.FullResultPath)
	if err != nil {
		t.Fatalf("read spilled blob: %v", err)
	}
	if string(content) != string(big) {
		t.Fatal("spilled blob content does not match the original oversized output")
	}
}

func TestSanitizeForFilename(t *testing.T) {
	got := sanitizeForFilename("platform/shell@v1")
	if strings.ContainsAny(got, "/@") {
		t.Fatalf("sanitizeForFilename(%q) = %q, still contains an unsafe character", "platform/shell@v1", got)
	}
}
