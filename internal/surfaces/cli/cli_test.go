package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMain_Run_ReportsRunIDAndWatchURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"run_id": "abc-123"})
	}))
	defer srv.Close()
	t.Setenv("NEXUS_HTTP_ADDR", srv.URL)
	t.Setenv("NEXUS_TENANT_ID", "t1")
	t.Setenv("NEXUS_USER_ID", "u1")

	var out, errOut bytes.Buffer
	code := Main([]string{"run", "do the thing"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("Main exit code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "run_id: abc-123") {
		t.Errorf("output = %q, want it to contain the run id", out.String())
	}
	if !strings.Contains(out.String(), srv.URL+"/v1/runs/abc-123") {
		t.Errorf("output = %q, want a watch URL naming the run", out.String())
	}
}

func TestMain_Run_RequiresExactlyOneArgument(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Main([]string{"run"}, &out, &errOut)
	if code == 0 {
		t.Fatal("Main(run with no input) = exit 0, want a non-zero exit")
	}
	if !strings.Contains(errOut.String(), "requires exactly one argument") {
		t.Errorf("stderr = %q, want a usage error", errOut.String())
	}
}

func TestMain_NoArgs_PrintsUsageAndVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Main(nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("Main(no args) exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "usage:") {
		t.Errorf("output = %q, want usage text", out.String())
	}
}

func TestMain_UnknownSubcommand_ExitsNonZero(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Main([]string{"bogus"}, &out, &errOut)
	if code == 0 {
		t.Fatal("Main(bogus subcommand) = exit 0, want non-zero")
	}
}

// TestApprovalsShow_RendersStructuredContext is this surface's conformance
// test (README task 7.12): Descriptor.CanRenderApprovalContext is true, so
// approvalsShow must actually print the full tool_id/effect_class/input
// breakdown capability.RenderApprovalContext produces for a
// full-capability descriptor — not fall back to the minimal one-liner a
// lower-capability surface would get.
func TestApprovalsShow_RendersStructuredContext(t *testing.T) {
	if !Descriptor.CanRenderApprovalContext || !Descriptor.SupportsStructuredInput {
		t.Fatal("Descriptor claims no structured approval-context rendering; this test assumes it does")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"approval_id": "ap-1",
			"context":     json.RawMessage(`{"tool_id":"platform/web_fetch@v1","effect_class":"external","input":{"url":"https://example.com"}}`),
		})
	}))
	defer srv.Close()
	t.Setenv("NEXUS_HTTP_ADDR", srv.URL)

	var out, errOut bytes.Buffer
	code := Main([]string{"approvals", "show", "ap-1"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("Main exit code = %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "platform/web_fetch@v1") || !strings.Contains(out.String(), "https://example.com") {
		t.Errorf("output = %q, want the full tool_id/input breakdown rendered, never a bare identifier", out.String())
	}
}

func TestDescriptor_ClaimsNoStreamingSupport(t *testing.T) {
	// Task 7.15's remaining growth item, honestly declared: nexusctl has
	// no `stream`/`events` subcommand yet, so Descriptor must say so.
	if Descriptor.SupportsStreaming {
		t.Error("Descriptor.SupportsStreaming = true, but no streaming subcommand exists in Main's switch")
	}
}
