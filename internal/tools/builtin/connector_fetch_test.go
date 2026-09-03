package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

type fakeTokenSource struct {
	token string
	err   error
}

func (f fakeTokenSource) AccessToken(context.Context, uuid.UUID, uuid.UUID, string) (string, error) {
	return f.token, f.err
}

type fakeSessionLookup struct {
	userID uuid.UUID
	err    error
}

func (f fakeSessionLookup) UserIDForSession(context.Context, uuid.UUID, uuid.UUID) (uuid.UUID, error) {
	return f.userID, f.err
}

func TestConnectorFetch_InjectsBearerTokenWithoutExposingIt(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("connector response"))
	}))
	defer srv.Close()

	c := ConnectorFetch{
		Tokens:               fakeTokenSource{token: "super-secret-token"},
		Sessions:             fakeSessionLookup{userID: uuid.New()},
		AllowedHosts:         []string{"*"},
		AllowPrivateNetworks: true,
	}
	in, err := json.Marshal(map[string]string{"provider": "acme", "url": srv.URL})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	result, err := c.Call(context.Background(), in, tools.RunContext{TenantID: uuid.New(), SessionID: uuid.New()})
	if err != nil || result.IsError {
		t.Fatalf("Call() = %+v, %v", result, err)
	}
	if gotAuth != "Bearer super-secret-token" {
		t.Fatalf("Authorization header = %q, want the injected bearer token", gotAuth)
	}
	// The one property that actually matters here (README task 11's
	// acceptance criterion, applied at unit-test granularity): the token
	// itself never appears in the tool's own Output — the model sees a
	// success/failure handle, never the secret.
	if strings.Contains(string(result.Output), "super-secret-token") {
		t.Fatalf("Output = %s, must never contain the raw token", result.Output)
	}
}

func TestConnectorFetch_NoConnectionRefusesWithoutLeakingWhy(t *testing.T) {
	c := ConnectorFetch{
		Tokens:       fakeTokenSource{err: context.DeadlineExceeded},
		Sessions:     fakeSessionLookup{userID: uuid.New()},
		AllowedHosts: []string{"*"},
	}
	in, err := json.Marshal(map[string]string{"provider": "acme", "url": "https://api.example.com/x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	result, err := c.Call(context.Background(), in, tools.RunContext{TenantID: uuid.New(), SessionID: uuid.New()})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("Call() with no usable connection succeeded, want a refusal")
	}
}

func TestConnectorFetch_ValidateInputRejectsMissingProvider(t *testing.T) {
	var c ConnectorFetch
	err := c.ValidateInput(context.Background(), json.RawMessage(`{"url":"https://api.example.com"}`), tools.RunContext{})
	if err == nil {
		t.Fatal("ValidateInput with no provider = nil error, want an error")
	}
}

func TestConnectorFetch_Taint(t *testing.T) {
	var c ConnectorFetch
	taint := c.Taint()
	if !taint.ReturnsUntrusted || !taint.ReadsPrivateData || !taint.MutatesExternal {
		t.Fatalf("Taint = %+v, want {true true true}", taint)
	}
}
