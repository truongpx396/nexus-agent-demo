package oauth2

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	xoauth2 "golang.org/x/oauth2"

	"github.com/truongpx396/nexus-agent-demo/internal/connectors"
)

// TestRegistry_Config is README task 13.8 (closing production-readiness finding
// F9): the pure registry lookup every BeginAuth call goes through first.
func TestRegistry_Config(t *testing.T) {
	reg := NewRegistry(Provider{
		Name: "github", ClientID: "cid", ClientSecret: "secret",
		Scopes: []string{"repo"}, RedirectURL: "https://example.com/callback",
	})

	cfg, ok := reg.config("github")
	if !ok {
		t.Fatal("config(\"github\") = not found, want the registered provider")
	}
	if cfg.ClientID != "cid" || cfg.ClientSecret != "secret" || cfg.RedirectURL != "https://example.com/callback" {
		t.Errorf("config = %+v, want it to carry the registered provider's fields", cfg)
	}
	if !reg.Known("github") || reg.Known("not-registered") {
		t.Error("Known should be true for the registered provider and false for any other")
	}
	if _, ok := reg.config("not-registered"); ok {
		t.Error("config(\"not-registered\") = found, want not found")
	}
}

func TestNewRegistry_Empty(t *testing.T) {
	reg := NewRegistry()
	if reg.Known("anything") {
		t.Error("an empty Registry should recognise nothing")
	}
	if _, err := reg.AuthCodeURL("anything", "s"); !errors.Is(err, connectors.ErrUnknownProvider) {
		t.Errorf("AuthCodeURL on an empty Registry = %v, want ErrUnknownProvider", err)
	}
	if _, err := reg.Exchange(context.Background(), "anything", "code"); !errors.Is(err, connectors.ErrUnknownProvider) {
		t.Errorf("Exchange on an empty Registry = %v, want ErrUnknownProvider", err)
	}
	if _, err := reg.Refresh(context.Background(), "anything", connectors.Token{}); !errors.Is(err, connectors.ErrUnknownProvider) {
		t.Errorf("Refresh on an empty Registry = %v, want ErrUnknownProvider", err)
	}
}

// TestRegistry_ExchangeAndRefreshSpeakToTheTokenEndpoint proves the conversion
// both ways against a fake token endpoint: the code exchange yields the issued
// pair, and a stale token is refreshed using its refresh token.
func TestRegistry_ExchangeAndRefreshSpeakToTheTokenEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		access := "from-" + r.Form.Get("grant_type")
		if r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") != "refresh-1" {
			http.Error(w, "wrong refresh token", http.StatusBadRequest)
			return
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer", "refresh_token": "refresh-1", "expires_in": 3600})
	}))
	defer srv.Close()
	reg := NewRegistry(Provider{Name: "acme", ClientID: "c", ClientSecret: "s", Endpoint: xoauth2.Endpoint{AuthURL: "https://acme.example/authorize", TokenURL: srv.URL}}) // fixture, no credential

	tok, err := reg.Exchange(context.Background(), "acme", "the-code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != "from-authorization_code" || tok.RefreshToken != "refresh-1" || tok.Expiry.Before(time.Now()) {
		t.Errorf("Exchange = %+v, want the issued pair with a future expiry", tok)
	}

	fresh, err := reg.Refresh(context.Background(), "acme", connectors.Token{AccessToken: "old", RefreshToken: "refresh-1", Expiry: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if fresh.AccessToken != "from-refresh_token" {
		t.Errorf("Refresh access token = %q, want the one the endpoint issued for the refresh grant", fresh.AccessToken)
	}

	url, err := reg.AuthCodeURL("acme", "state-xyz")
	if err != nil || url == "" {
		t.Fatalf("AuthCodeURL = %q, %v", url, err)
	}
}
