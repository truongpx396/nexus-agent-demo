//go:build integration

package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/oauth2"

	"github.com/truongpx396/nexus-agent-demo/internal/config"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// setupConnectorsEnv mirrors tests/integration/cost_ceiling_test.go's own
// setupCostEnv (postgres + redis, this codebase's established per-file
// duplication idiom) — the OAuth vault needs both: postgres for
// oauth_connections/tenant_configs, redis for the bounded-TTL CSRF state.
func setupConnectorsEnv(t *testing.T) (*pgxpool.Pool, *goredis.Client) {
	t.Helper()
	ctx := context.Background()

	pgReq := testcontainers.ContainerRequest{
		Image:        "postgres:17",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "nexus",
			"POSTGRES_PASSWORD": "nexus",
			"POSTGRES_DB":       "nexus",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(120 * time.Second),
	}
	pgC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: pgReq, Started: true})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })

	redisC, err := tcredis.Run(ctx, "redis:7")
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() { _ = redisC.Terminate(ctx) })

	pgHost, err := pgC.Host(ctx)
	if err != nil {
		t.Fatalf("postgres host: %v", err)
	}
	pgPort, err := pgC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("postgres mapped port: %v", err)
	}

	migrateDSN := fmt.Sprintf("postgres://nexus:nexus@%s:%s/nexus", pgHost, pgPort.Port())
	migratePool, err := pgxpool.New(ctx, migrateDSN)
	if err != nil {
		t.Fatalf("connect as migration role: %v", err)
	}
	defer migratePool.Close()
	if _, err := store.Migrate(ctx, migratePool, migrations.FS); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	appDSN := fmt.Sprintf("postgres://nexus_app:nexus_app@%s:%s/nexus", pgHost, pgPort.Port())
	appPool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect as nexus_app: %v", err)
	}
	t.Cleanup(appPool.Close)

	redisConnStr, err := redisC.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis connection string: %v", err)
	}
	opts, err := goredis.ParseURL(redisConnStr)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	redisClient := goredis.NewClient(opts)
	t.Cleanup(func() { _ = redisClient.Close() })
	return appPool, redisClient
}

// fakeTokenEndpoint serves a minimal OAuth2 authorization-code token
// endpoint: any code exchanges for a fresh token; a refresh_token grant
// returns a NEW access token (so the refresh-flow test can tell the two
// apart) and echoes the refresh token back unchanged, matching what a real
// provider that doesn't rotate refresh tokens does. respondFor computes
// (access_token, expires_in) per grant type, since a test proving "the
// refreshed token is reused, not re-refreshed" needs the INITIAL exchange
// to expire immediately while the REFRESH response does not.
func fakeTokenEndpoint(t *testing.T, respondFor func(grantType string) (accessToken string, expiresIn int)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		accessToken, expiresIn := respondFor(r.Form.Get("grant_type"))
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  accessToken,
			"token_type":    "Bearer",
			"refresh_token": "refresh-abc",
			"expires_in":    expiresIn,
		})
	}))
}

func insertTenant(t *testing.T, s *store.Store, tenantID uuid.UUID) {
	t.Helper()
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'connectors-test')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
}

func admitProvider(t *testing.T, s *store.Store, tenantID uuid.UUID, provider string) {
	t.Helper()
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return config.Upsert(ctx, tx, config.TenantConfig{TenantID: tenantID, AdmittedConnectorProviders: []string{provider}})
	}); err != nil {
		t.Fatalf("admit provider: %v", err)
	}
}

func TestVault_BeginAuthRefusesUnadmittedProvider(t *testing.T) {
	pool, redisClient := setupConnectorsEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	insertTenant(t, s, tenantID)
	// Deliberately never admitted for this tenant.

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	v := &Vault{
		Store: s, Keys: crypto.NewKeyStore(kek), Redis: redisClient,
		Providers: NewRegistry(Provider{Name: "acme", Endpoint: oauth2.Endpoint{AuthURL: "https://acme.example/authorize", TokenURL: "https://acme.example/token"}}),
	}
	if _, err := v.BeginAuth(context.Background(), tenantID, uuid.New(), "acme"); err == nil {
		t.Fatal("BeginAuth for an unadmitted provider succeeded, want a refusal")
	}
}

func TestVault_FullRoundTrip_BeginAuthCallbackThenToken(t *testing.T) {
	pool, redisClient := setupConnectorsEnv(t)
	s := store.New(pool)
	tenantID, userID := uuid.New(), uuid.New()
	insertTenant(t, s, tenantID)
	admitProvider(t, s, tenantID, "acme")

	tokenSrv := fakeTokenEndpoint(t, func(string) (string, int) { return "access-token-1", 3600 })
	defer tokenSrv.Close()

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	v := &Vault{
		Store: s, Keys: crypto.NewKeyStore(kek), Redis: redisClient,
		Providers: NewRegistry(Provider{Name: "acme", Endpoint: oauth2.Endpoint{AuthURL: "https://acme.example/authorize", TokenURL: tokenSrv.URL}, ClientID: "client-1", ClientSecret: "secret-1"}),
	}

	redirectURL, err := v.BeginAuth(context.Background(), tenantID, userID, "acme")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	u, err := url.Parse(redirectURL)
	if err != nil {
		t.Fatalf("parse redirect url: %v", err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatal("BeginAuth's redirect URL carries no state parameter")
	}

	if err := v.HandleCallback(context.Background(), state, "auth-code-xyz"); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}

	// A replay of the SAME state must fail closed — the whole point of
	// single-use consumption (luaGetAndDelete's own doc comment).
	if err := v.HandleCallback(context.Background(), state, "auth-code-xyz"); err == nil {
		t.Fatal("HandleCallback replayed the same state successfully, want a refusal")
	}

	tok, err := v.AccessToken(context.Background(), tenantID, userID, "acme")
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok != "access-token-1" {
		t.Fatalf("AccessToken = %q, want the token the fake provider issued", tok)
	}

	// The stored row must never carry the token in the clear.
	var sealed []byte
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT sealed_access_token FROM oauth_connections WHERE tenant_id = $1 AND user_id = $2 AND provider = 'acme'`, tenantID, userID).Scan(&sealed)
	}); err != nil {
		t.Fatalf("load sealed row: %v", err)
	}
	if containsPlaintext(sealed, "access-token-1") {
		t.Fatal("sealed_access_token contains the plaintext token — envelope encryption did not apply")
	}
}

func containsPlaintext(sealed []byte, plain string) bool {
	s := string(sealed)
	for i := 0; i+len(plain) <= len(s); i++ {
		if s[i:i+len(plain)] == plain {
			return true
		}
	}
	return false
}

func TestVault_TokenRefreshesWhenExpired(t *testing.T) {
	pool, redisClient := setupConnectorsEnv(t)
	s := store.New(pool)
	tenantID, userID := uuid.New(), uuid.New()
	insertTenant(t, s, tenantID)
	admitProvider(t, s, tenantID, "acme")

	calls := 0
	tokenSrv := fakeTokenEndpoint(t, func(grantType string) (string, int) {
		calls++
		if grantType == "refresh_token" {
			// The REFRESHED token is genuinely valid — a second AccessToken
			// call must reuse it from storage, not refresh again.
			return "refreshed-access-token", 3600
		}
		// The INITIAL exchange's token expires immediately, forcing the
		// first AccessToken call to refresh right away.
		return "initial-access-token", -1
	})
	defer tokenSrv.Close()

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	v := &Vault{
		Store: s, Keys: crypto.NewKeyStore(kek), Redis: redisClient,
		Providers: NewRegistry(Provider{Name: "acme", Endpoint: oauth2.Endpoint{AuthURL: "https://acme.example/authorize", TokenURL: tokenSrv.URL}, ClientID: "client-1", ClientSecret: "secret-1"}),
	}

	redirectURL, err := v.BeginAuth(context.Background(), tenantID, userID, "acme")
	if err != nil {
		t.Fatalf("BeginAuth: %v", err)
	}
	u, err := url.Parse(redirectURL)
	if err != nil {
		t.Fatalf("parse redirect url: %v", err)
	}
	if err := v.HandleCallback(context.Background(), u.Query().Get("state"), "auth-code-xyz"); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}

	tok, err := v.AccessToken(context.Background(), tenantID, userID, "acme")
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok != "refreshed-access-token" {
		t.Fatalf("AccessToken = %q, want the REFRESHED token (expired initial token must trigger a refresh)", tok)
	}
	if calls != 2 { // 1 initial exchange (authorization_code) + 1 refresh
		t.Fatalf("token endpoint called %d times, want 2 (initial exchange + refresh)", calls)
	}

	// A second call must reuse the now-fresh token from storage rather than
	// refreshing again — Token.Valid() with a real future expiry short-circuits.
	if _, err := v.AccessToken(context.Background(), tenantID, userID, "acme"); err != nil {
		t.Fatalf("AccessToken (second call): %v", err)
	}
	if calls != 2 {
		t.Fatalf("token endpoint called %d times after a second AccessToken call, want still 2 (no redundant refresh)", calls)
	}
}
