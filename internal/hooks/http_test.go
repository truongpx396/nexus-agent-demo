package hooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPHandler_SSRFGuardRefusesPrivateAddress(t *testing.T) {
	h := HTTPHandler{}
	cfg := Config{Name: "ssrf", Kind: KindHTTP, URL: "http://127.0.0.1:9999/hook"}
	_, err := h.Run(context.Background(), cfg, Context{ToolID: "x/y@v1"})
	if err == nil {
		t.Fatal("Run() = nil error, want a refusal for a loopback address")
	}
}

func TestHTTPHandler_RejectsNonHTTPScheme(t *testing.T) {
	h := HTTPHandler{}
	cfg := Config{Name: "bad-scheme", Kind: KindHTTP, URL: "file:///etc/passwd"}
	_, err := h.Run(context.Background(), cfg, Context{ToolID: "x/y@v1"})
	if err == nil {
		t.Fatal("Run() = nil error, want a refusal for a non-http(s) scheme")
	}
}

func TestHTTPHandler_SuccessfulCallParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req commandRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("server: decode request: %v", err)
		}
		if req.ToolID != "platform/shell@v1" {
			t.Errorf("server saw tool_id %q, want platform/shell@v1", req.ToolID)
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(commandResponse{Decision: "ask", Reason: "please confirm"})
	}))
	defer srv.Close()

	h := HTTPHandler{AllowPrivateNetworks: true}
	cfg := Config{Name: "ok", Kind: KindHTTP, URL: srv.URL}
	out, err := h.Run(context.Background(), cfg, Context{ToolID: "platform/shell@v1"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.Decision != Ask || out.Reason != "please confirm" {
		t.Fatalf("out = %+v, want Decision=ask Reason=%q", out, "please confirm")
	}
}
