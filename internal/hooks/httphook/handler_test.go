package httphook

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/nexus-agent-demo/internal/hooks"
)

func TestHandler_SSRFGuardRefusesPrivateAddress(t *testing.T) {
	h := Handler{}
	cfg := hooks.Config{Name: "ssrf", Kind: hooks.KindHTTP, URL: "http://127.0.0.1:9999/hook"}
	_, err := h.Run(context.Background(), cfg, hooks.Context{ToolID: "x/y@v1"})
	if err == nil {
		t.Fatal("Run() = nil error, want a refusal for a loopback address")
	}
}

func TestHandler_RejectsNonHTTPScheme(t *testing.T) {
	h := Handler{}
	cfg := hooks.Config{Name: "bad-scheme", Kind: hooks.KindHTTP, URL: "file:///etc/passwd"}
	_, err := h.Run(context.Background(), cfg, hooks.Context{ToolID: "x/y@v1"})
	if err == nil {
		t.Fatal("Run() = nil error, want a refusal for a non-http(s) scheme")
	}
}

func TestHandler_SuccessfulCallParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req hooks.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("server: decode request: %v", err)
		}
		if req.ToolID != "platform/shell@v1" {
			t.Errorf("server saw tool_id %q, want platform/shell@v1", req.ToolID)
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(hooks.Response{Decision: "ask", Reason: "please confirm"})
	}))
	defer srv.Close()

	h := Handler{AllowPrivateNetworks: true}
	cfg := hooks.Config{Name: "ok", Kind: hooks.KindHTTP, URL: srv.URL}
	out, err := h.Run(context.Background(), cfg, hooks.Context{ToolID: "platform/shell@v1"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.Decision != hooks.Ask || out.Reason != "please confirm" {
		t.Fatalf("out = %+v, want hooks.Decision=ask Reason=%q", out, "please confirm")
	}
}

func TestIsPrivateIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.0.0.1": true, "192.168.0.1": true, "169.254.169.254": true,
		"100.64.0.1": true, "198.18.0.1": true, "240.0.0.1": true, "64:ff9b::7f00:1": true, "224.0.0.1": true,
		"8.8.8.8": false, "2606:4700:4700::1111": false,
	} {
		if got := isPrivateIP(net.ParseIP(ip)); got != want {
			t.Errorf("isPrivateIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

// The dialer vets the address actually connected to, so a hook client built with
// no private networks allowed cannot reach a loopback endpoint however it got there.
func TestEgressClient_RefusesAPrivateAddressWhenDialling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	_, err := newEgressClient(false, 2*time.Second).Get(srv.URL) //nolint:noctx // a bounded test request
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("Get(%s) error = %v, want a refusal that mentions the address", srv.URL, err)
	}
}

// A hook is configured with one host and only talks to it: a redirect elsewhere
// would let the endpoint aim the call at something the operator never named.
func TestHandler_ARedirectToAnotherHostIsRefused(t *testing.T) {
	var viaLocalhost string // the same server, under a name that is not the configured host
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inside" {
			t.Error("the redirect target was requested")
		}
		http.Redirect(w, r, viaLocalhost+"/inside", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	viaLocalhost = "http://localhost:" + port

	h := Handler{AllowPrivateNetworks: true}
	_, err = h.Run(context.Background(), hooks.Config{Name: "redir", Kind: hooks.KindHTTP, URL: srv.URL + "/hook"}, hooks.Context{ToolID: "x/y@v1"})
	if err == nil || !strings.Contains(err.Error(), "only talks to the host it is configured with") {
		t.Fatalf("Run() error = %v, want the redirect refused", err)
	}
}

// A redirect that stays on the configured host is fine.
func TestHandler_ARedirectWithinTheSameHostIsFollowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hook" {
			http.Redirect(w, r, "/moved", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(hooks.Response{Decision: "allow"})
	}))
	defer srv.Close()

	out, err := Handler{AllowPrivateNetworks: true}.Run(context.Background(), hooks.Config{Name: "same", Kind: hooks.KindHTTP, URL: srv.URL + "/hook"}, hooks.Context{ToolID: "x/y@v1"})
	if err != nil || out.Decision != hooks.Allow {
		t.Fatalf("Run() = %+v, %v, want allow", out, err)
	}
}
