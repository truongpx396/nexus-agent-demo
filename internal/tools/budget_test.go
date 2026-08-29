package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestBudgetResult_UnderCapPassesThrough(t *testing.T) {
	blobs := BlobStore{Dir: t.TempDir()}
	small := json.RawMessage(`{"ok":true}`)
	out, err := BudgetResult(context.Background(), blobs, uuid.New(), uuid.New(), "x/y@v1", small, nil)
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
	tenantID := uuid.New()
	sessionID := uuid.New()

	var recorded []string
	recorder := func(_ context.Context, gotTenant, gotSession uuid.UUID, kind, path string) error {
		if gotTenant != tenantID || gotSession != sessionID {
			t.Fatalf("recorder called with tenant=%s session=%s, want tenant=%s session=%s", gotTenant, gotSession, tenantID, sessionID)
		}
		if kind != "blob" {
			t.Fatalf("recorder kind = %q, want %q", kind, "blob")
		}
		recorded = append(recorded, path)
		return nil
	}

	out, err := BudgetResult(context.Background(), blobs, tenantID, sessionID, "platform/shell@v1", big, recorder)
	if err != nil {
		t.Fatalf("BudgetResult: %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("recorder called %d times, want 1", len(recorded))
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
