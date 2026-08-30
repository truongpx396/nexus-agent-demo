package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

func TestWebFetch_SSRFGuardRefusesLoopback(t *testing.T) {
	w := WebFetch{}
	result, err := w.Call(context.Background(), json.RawMessage(`{"url":"http://127.0.0.1:9/x"}`), tools.RunContext{})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("Call() to a loopback address succeeded, want a refusal")
	}
}

func TestWebFetch_ValidateInputRejectsNonHTTPScheme(t *testing.T) {
	var w WebFetch
	if err := w.ValidateInput(context.Background(), json.RawMessage(`{"url":"file:///etc/passwd"}`), tools.RunContext{}); err == nil {
		t.Fatal("ValidateInput(file://) = nil error, want an error")
	}
}

func TestWebFetch_SuccessfulFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello from the test server"))
	}))
	defer srv.Close()

	w := WebFetch{AllowPrivateNetworks: true, AllowedHosts: []string{"*"}}
	in, err := json.Marshal(map[string]string{"url": srv.URL})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := w.ValidateInput(context.Background(), in, tools.RunContext{}); err != nil {
		t.Fatalf("ValidateInput: %v", err)
	}
	result, err := w.Call(context.Background(), in, tools.RunContext{})
	if err != nil || result.IsError {
		t.Fatalf("Call() = %+v, %v", result, err)
	}
	var decoded struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Status != http.StatusOK || decoded.Body != "hello from the test server" {
		t.Fatalf("decoded = %+v, want status 200 and the server's body", decoded)
	}
}

func TestWebFetch_Taint(t *testing.T) {
	var w WebFetch
	taint := w.Taint()
	if !taint.ReturnsUntrusted || taint.ReadsPrivateData || !taint.MutatesExternal {
		t.Fatalf("Taint = %+v, want {true false true}", taint)
	}
	if w.Descriptor().EffectClass != tools.EffectClassExternal {
		t.Fatalf("EffectClass = %v, want external", w.Descriptor().EffectClass)
	}
}
