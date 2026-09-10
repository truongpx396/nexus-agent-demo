package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

func TestWebCrawl_TaintMatchesWebFetch(t *testing.T) {
	// docs/build-phases.md Phase 16, task 16.2's own acceptance test:
	// delegating the fetch to Crawl4AI must never delegate the trust
	// decision — the taint declaration must be identical, field for field.
	crawlTaint := WebCrawl{}.Taint()
	fetchTaint := WebFetch{}.Taint()
	if crawlTaint != fetchTaint {
		t.Fatalf("WebCrawl.Taint() = %+v, want it to match WebFetch.Taint() = %+v", crawlTaint, fetchTaint)
	}
	if ec := (WebCrawl{}).Descriptor().EffectClass; ec != tools.EffectClassExternal {
		t.Fatalf("EffectClass = %v, want external", ec)
	}
}

func TestWebCrawl_ValidateInputRejectsDisallowedHost(t *testing.T) {
	w := WebCrawl{AllowedHosts: []string{"example.com"}}
	err := w.ValidateInput(context.Background(), json.RawMessage(`{"url":"https://not-allowed.test/x"}`), tools.RunContext{})
	if err == nil {
		t.Fatal("ValidateInput() = nil error for a host not on the allowlist, want an error")
	}
}

func TestWebCrawl_ValidateInputRejectsNonHTTPScheme(t *testing.T) {
	w := WebCrawl{AllowedHosts: []string{"*"}}
	if err := w.ValidateInput(context.Background(), json.RawMessage(`{"url":"file:///etc/passwd"}`), tools.RunContext{}); err == nil {
		t.Fatal("ValidateInput(file://) = nil error, want an error")
	}
}

func TestWebCrawl_CallWithoutBaseURLFailsClosed(t *testing.T) {
	w := WebCrawl{AllowedHosts: []string{"*"}}
	in, _ := json.Marshal(map[string]string{"url": "https://example.com"})
	result, err := w.Call(context.Background(), in, tools.RunContext{})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("Call() with no BaseURL configured succeeded, want a refusal")
	}
}

func TestWebCrawl_SuccessfulCrawl(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/md" {
			t.Errorf("request path = %q, want /md", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer test-token")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body["url"] != "https://example.com/article" {
			t.Errorf("request url = %q, want https://example.com/article", body["url"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"url": body["url"], "markdown": "# Article\n\nClean content.", "success": true,
		})
	}))
	defer srv.Close()

	w := WebCrawl{BaseURL: srv.URL, APIToken: "test-token", AllowedHosts: []string{"*"}}
	in, _ := json.Marshal(map[string]string{"url": "https://example.com/article"})
	result, err := w.Call(context.Background(), in, tools.RunContext{})
	if err != nil || result.IsError {
		t.Fatalf("Call() = %+v, %v", result, err)
	}
	var decoded struct {
		URL      string `json:"url"`
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Markdown != "# Article\n\nClean content." {
		t.Fatalf("decoded.Markdown = %q, want the server's markdown", decoded.Markdown)
	}
}

func TestWebCrawl_UpstreamErrorSurfacesAsResultError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"invalid token"}`))
	}))
	defer srv.Close()

	w := WebCrawl{BaseURL: srv.URL, AllowedHosts: []string{"*"}}
	in, _ := json.Marshal(map[string]string{"url": "https://example.com"})
	result, err := w.Call(context.Background(), in, tools.RunContext{})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("Call() against a 401 upstream succeeded, want a refusal")
	}
}
