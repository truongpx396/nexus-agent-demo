package connectors_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/truongpx396/nexus-agent-demo/internal/config"
	"github.com/truongpx396/nexus-agent-demo/internal/connectors"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
)

// fakeConfig admits the providers it is given, for every tenant.
type fakeConfig struct{ admitted []string }

func (f fakeConfig) Load(_ context.Context, tenantID uuid.UUID) (config.TenantConfig, error) {
	return config.TenantConfig{TenantID: tenantID, AdmittedConnectorProviders: f.admitted}, nil
}

// fakeStates is a single-use state store: Take removes what it returns.
type fakeStates struct {
	mu   sync.Mutex
	m    map[string][]byte
	ttls map[string]time.Duration
}

func newFakeStates() *fakeStates {
	return &fakeStates{m: map[string][]byte{}, ttls: map[string]time.Duration{}}
}

func (f *fakeStates) Put(_ context.Context, state string, raw []byte, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[state], f.ttls[state] = raw, ttl
	return nil
}

func (f *fakeStates) Take(_ context.Context, state string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.m[state]
	delete(f.m, state)
	return raw, ok, nil
}

// fakeConnections keeps the sealed bytes exactly as the vault produced them,
// under one data key it mints itself, so tests can inspect what would be
// stored.
type fakeConnections struct {
	dek   crypto.DEK
	saved map[string]connectors.StoredConnection
	saves int
}

func newFakeConnections(t *testing.T) *fakeConnections {
	t.Helper()
	dek, err := crypto.GenerateDEK("test-key")
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	return &fakeConnections{dek: dek, saved: map[string]connectors.StoredConnection{}}
}

func connKey(tenantID, userID uuid.UUID, provider string) string {
	return tenantID.String() + "|" + userID.String() + "|" + provider
}

func (f *fakeConnections) Save(_ context.Context, tenantID, userID uuid.UUID, provider string, expiresAt *time.Time,
	seal func(dek crypto.DEK) (access, refresh []byte, err error)) error {
	access, refresh, err := seal(f.dek)
	if err != nil {
		return err
	}
	f.saves++
	f.saved[connKey(tenantID, userID, provider)] = connectors.StoredConnection{SealedAccess: access, SealedRefresh: refresh, ExpiresAt: expiresAt, DEK: f.dek}
	return nil
}

func (f *fakeConnections) Load(_ context.Context, tenantID, userID uuid.UUID, provider string) (connectors.StoredConnection, error) {
	c, ok := f.saved[connKey(tenantID, userID, provider)]
	if !ok {
		return connectors.StoredConnection{}, errors.New("load connection: no rows")
	}
	return c, nil
}

// tokenServer is a provider's token endpoint: respond picks the access token
// and its lifetime for each grant type.
func tokenServer(t *testing.T, respond func(grantType string) (accessToken string, expiresIn int)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		access, expiresIn := respond(r.Form.Get("grant_type"))
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer", "refresh_token": "refresh-abc", "expires_in": expiresIn})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newVault(t *testing.T, admitted []string, tokenURL string) (*connectors.Vault, *fakeStates, *fakeConnections) {
	t.Helper()
	states, conns := newFakeStates(), newFakeConnections(t)
	return &connectors.Vault{
		Config: fakeConfig{admitted: admitted}, Connections: conns, States: states,
		Providers: connectors.NewRegistry(connectors.Provider{
			Name: "acme", ClientID: "client-1", ClientSecret: "secret-1", //nolint:gosec // fixture, no credential
			Endpoint: oauth2.Endpoint{AuthURL: "https://acme.example/authorize", TokenURL: tokenURL},
		}),
	}, states, conns
}

func stateOf(t *testing.T, redirect string) string {
	t.Helper()
	u, err := url.Parse(redirect)
	if err != nil || u.Query().Get("state") == "" {
		t.Fatalf("redirect %q carries no state (%v)", redirect, err)
	}
	return u.Query().Get("state")
}

func TestBeginAuth_RefusesAProviderTheTenantHasNotAdmitted(t *testing.T) {
	v, states, _ := newVault(t, nil, "https://acme.example/token")
	if _, err := v.BeginAuth(context.Background(), uuid.New(), uuid.New(), "acme"); err == nil {
		t.Fatal("BeginAuth for an unadmitted provider succeeded: being a known provider is necessary, never sufficient")
	}
	if len(states.m) != 0 {
		t.Errorf("%d states stored for a refused flow, want none", len(states.m))
	}
}

func TestBeginAuth_RefusesAnAdmittedButUnrecognizedProvider(t *testing.T) {
	v, _, _ := newVault(t, []string{"ghost"}, "https://acme.example/token")
	if _, err := v.BeginAuth(context.Background(), uuid.New(), uuid.New(), "ghost"); err == nil {
		t.Fatal("BeginAuth for a provider this deployment does not know succeeded")
	}
}

func TestBeginAuth_StoresTheStateWithTheConfiguredTTL(t *testing.T) {
	v, states, _ := newVault(t, []string{"acme"}, "https://acme.example/token")
	redirect, err := v.BeginAuth(context.Background(), uuid.New(), uuid.New(), "acme")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	state := stateOf(t, redirect)
	if states.ttls[state] != 5*time.Minute {
		t.Errorf("state ttl = %s, want the 5 minute default: an abandoned flow must not leave state behind for ever", states.ttls[state])
	}
	v.StateTTL = time.Minute
	redirect, _ = v.BeginAuth(context.Background(), uuid.New(), uuid.New(), "acme")
	if got := states.ttls[stateOf(t, redirect)]; got != time.Minute {
		t.Errorf("state ttl = %s, want the configured minute", got)
	}
}

func TestHandleCallback_ConsumesTheStateOnceAndSealsTheTokens(t *testing.T) {
	srv := tokenServer(t, func(string) (string, int) { return "access-token-1", 3600 })
	v, _, conns := newVault(t, []string{"acme"}, srv.URL)
	tenantID, userID := uuid.New(), uuid.New()

	redirect, err := v.BeginAuth(context.Background(), tenantID, userID, "acme")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	state := stateOf(t, redirect)
	if err := v.HandleCallback(context.Background(), state, "code"); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if err := v.HandleCallback(context.Background(), state, "code"); err == nil {
		t.Fatal("a replayed callback succeeded: the state must be single-use")
	}

	saved := conns.saved[connKey(tenantID, userID, "acme")]
	if len(saved.SealedAccess) == 0 || bytes.Contains(saved.SealedAccess, []byte("access-token-1")) {
		t.Errorf("stored access token = %q, want it present and sealed, never the plaintext", saved.SealedAccess)
	}
	if len(saved.SealedRefresh) == 0 || bytes.Contains(saved.SealedRefresh, []byte("refresh-abc")) {
		t.Errorf("stored refresh token = %q, want it present and sealed, never the plaintext", saved.SealedRefresh)
	}
	if got, err := v.AccessToken(context.Background(), tenantID, userID, "acme"); err != nil || got != "access-token-1" {
		t.Errorf("AccessToken = %q, %v, want the token the provider issued", got, err)
	}
}

func TestHandleCallback_AnUnknownStateIsRefused(t *testing.T) {
	v, _, conns := newVault(t, []string{"acme"}, "https://acme.example/token")
	if err := v.HandleCallback(context.Background(), "never-issued", "code"); err == nil {
		t.Fatal("a callback with a state this vault never issued succeeded")
	}
	if conns.saves != 0 {
		t.Errorf("%d connections saved for an unknown state, want none", conns.saves)
	}
}

func TestToken_RefreshesAnExpiredTokenOnceAndReusesTheFreshOne(t *testing.T) {
	calls := 0
	srv := tokenServer(t, func(grant string) (string, int) {
		calls++
		if grant == "refresh_token" {
			return "refreshed-token", 3600
		}
		return "initial-token", -1 // already expired, so the first read must refresh
	})
	v, _, conns := newVault(t, []string{"acme"}, srv.URL)
	tenantID, userID := uuid.New(), uuid.New()

	redirect, err := v.BeginAuth(context.Background(), tenantID, userID, "acme")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	if err := v.HandleCallback(context.Background(), stateOf(t, redirect), "code"); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}

	for i := 0; i < 2; i++ {
		if got, err := v.AccessToken(context.Background(), tenantID, userID, "acme"); err != nil || got != "refreshed-token" {
			t.Fatalf("AccessToken call %d = %q, %v, want the refreshed token", i+1, got, err)
		}
	}
	if calls != 2 {
		t.Errorf("token endpoint called %d times, want 2 (exchange + one refresh): a fresh token is reused, not refreshed again", calls)
	}
	if conns.saves != 2 {
		t.Errorf("connections saved %d times, want 2 (exchange + the re-sealed refresh)", conns.saves)
	}
}

func TestToken_AConnectionThatDoesNotExistIsAnError(t *testing.T) {
	v, _, _ := newVault(t, []string{"acme"}, "https://acme.example/token")
	if _, err := v.Token(context.Background(), uuid.New(), uuid.New(), "acme"); err == nil {
		t.Fatal("Token for a user who never connected succeeded")
	}
}
