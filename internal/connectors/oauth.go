// Package connectors is the per-user OAuth token vault (README Phase 11,
// task 11.2): an authorization-code flow per (tenant_id, user_id, provider),
// with the resulting tokens sealed under the SAME per-tenant envelope
// encryption every other secret in this system uses (internal/crypto,
// pattern #32) — never placed in an event payload, a log line, or a span in
// plaintext. Vault.Token/AccessToken are readable ONLY from inside a
// connector tool's own Call (docs/constitution.md: "the model sees only a
// handle"); nothing in this package ever hands a raw token to anything that
// isn't about to use it in an outbound request.
package connectors

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/truongpx396/nexus-agent-demo/internal/config"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
)

// Provider is one OAuth2 authorization-code provider this DEPLOYMENT
// recognizes — a small, operator-configured, process-wide registry
// (cmd/nexusd wires it from env vars), never a runtime user-editable list.
// Which providers EXIST is platform config; which providers a given TENANT
// may actually use is config.TenantConfig.AdmittedConnectorProviders — the
// vetted, per-tenant catalog docs/constitution.md requires
// ("Connectors MUST attach only through the vetted, per-tenant ...
// catalog"). Two distinct admission checks, on purpose: existing-but-not-
// admitted-here and admitted-but-doesn't-exist are different failures, both
// fail closed.
type Provider struct {
	Name         string
	Endpoint     oauth2.Endpoint
	ClientID     string
	ClientSecret string
	Scopes       []string
	RedirectURL  string
}

// Registry is the static set of recognized providers.
type Registry struct {
	providers map[string]Provider
}

func NewRegistry(providers ...Provider) *Registry {
	m := make(map[string]Provider, len(providers))
	for _, p := range providers {
		m[p.Name] = p
	}
	return &Registry{providers: m}
}

func (r *Registry) oauth2Config(name string) (oauth2.Config, bool) {
	p, ok := r.providers[name]
	if !ok {
		return oauth2.Config{}, false
	}
	return oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		Endpoint:     p.Endpoint,
		Scopes:       p.Scopes,
		RedirectURL:  p.RedirectURL,
	}, true
}

// Vault is the per-user OAuth token vault: Connections (migrations/
// 0018_oauth_connections.sql) holds the sealed tokens, and States — a
// bounded-TTL store, not a durable table, since CSRF state lives minutes, not
// the lifetime of a connection, and needs no audit durability — carries the
// authorization-code flow's state parameter between BeginAuth and
// HandleCallback.
type Vault struct {
	Config      config.Loader
	Connections Connections
	States      StateStore
	Providers   *Registry
	StateTTL    time.Duration // default 5 minutes
}

// stateRecord is what BeginAuth stashes in Redis under the state token —
// enough for HandleCallback to resume without trusting anything the
// provider's redirect itself carries beyond state/code.
type stateRecord struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	Provider string `json:"provider"`
}

func randomState() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// BeginAuth starts an authorization-code flow for (tenantID, userID,
// providerName). Refuses fail-closed unless the TENANT has actually
// admitted this provider (config.TenantConfig.HasConnectorProvider) —
// being a recognized Provider in the process-wide Registry is necessary
// but never sufficient.
func (v *Vault) BeginAuth(ctx context.Context, tenantID, userID uuid.UUID, providerName string) (redirectURL string, err error) {
	cfg, err := v.Config.Load(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("connectors: load tenant config: %w", err)
	}
	if !cfg.HasConnectorProvider(providerName) {
		return "", fmt.Errorf("connectors: provider %q is not admitted for tenant %s", providerName, tenantID)
	}
	oc, ok := v.Providers.oauth2Config(providerName)
	if !ok {
		return "", fmt.Errorf("connectors: unrecognized provider %q", providerName)
	}

	state, err := randomState()
	if err != nil {
		return "", fmt.Errorf("connectors: generate state: %w", err)
	}
	raw, err := json.Marshal(stateRecord{TenantID: tenantID.String(), UserID: userID.String(), Provider: providerName})
	if err != nil {
		return "", fmt.Errorf("connectors: marshal state: %w", err)
	}
	ttl := v.StateTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if err := v.States.Put(ctx, state, raw, ttl); err != nil {
		return "", err
	}
	return oc.AuthCodeURL(state), nil
}

// HandleCallback completes the authorization-code exchange for state/code,
// seals the resulting token pair under the tenant's own DEK, and upserts
// oauth_connections. state is consumed exactly once.
func (v *Vault) HandleCallback(ctx context.Context, state, code string) error {
	raw, found, err := v.States.Take(ctx, state)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("connectors: state %q is unknown, expired, or already consumed", state)
	}
	var rec stateRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("connectors: unmarshal state: %w", err)
	}
	tenantID, err := uuid.Parse(rec.TenantID)
	if err != nil {
		return fmt.Errorf("connectors: state tenant_id: %w", err)
	}
	userID, err := uuid.Parse(rec.UserID)
	if err != nil {
		return fmt.Errorf("connectors: state user_id: %w", err)
	}

	oc, ok := v.Providers.oauth2Config(rec.Provider)
	if !ok {
		return fmt.Errorf("connectors: unrecognized provider %q", rec.Provider)
	}
	tok, err := oc.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("connectors: exchange code: %w", err)
	}
	return v.persist(ctx, tenantID, userID, rec.Provider, tok)
}

// aadLabel binds (user_id, provider) as the second half of Seal/Open's AAD
// — internal/crypto.Seal's own signature takes (tenantID, sessionID
// string), but an OAuth connection has no session; this is just a binding
// LABEL, not literally a session, the same repurposing outbox.go's own
// activeKeyID lookup avoids needing by having an actual session to hand.
func aadLabel(userID uuid.UUID, providerName string) string {
	return userID.String() + "|" + providerName
}

func (v *Vault) persist(ctx context.Context, tenantID, userID uuid.UUID, providerName string, tok *oauth2.Token) error {
	var expiresAt *time.Time
	if !tok.Expiry.IsZero() {
		e := tok.Expiry
		expiresAt = &e
	}
	return v.Connections.Save(ctx, tenantID, userID, providerName, expiresAt, func(dek crypto.DEK) (access, refresh []byte, err error) {
		aad := aadLabel(userID, providerName)
		access, err = crypto.Seal(dek, []byte(tok.AccessToken), tenantID.String(), aad)
		if err != nil {
			return nil, nil, fmt.Errorf("seal access token: %w", err)
		}
		if tok.RefreshToken != "" {
			refresh, err = crypto.Seal(dek, []byte(tok.RefreshToken), tenantID.String(), aad)
			if err != nil {
				return nil, nil, fmt.Errorf("seal refresh token: %w", err)
			}
		}
		return access, refresh, nil
	})
}

// Token returns a live, usable token for (tenantID, userID, providerName),
// transparently refreshing (and re-sealing) it first if it has expired.
// Callers: ONLY a connector tool's own Call — never logged, never placed in
// an event payload, never returned in a tools.Result.Output.
func (v *Vault) Token(ctx context.Context, tenantID, userID uuid.UUID, providerName string) (*oauth2.Token, error) {
	oc, ok := v.Providers.oauth2Config(providerName)
	if !ok {
		return nil, fmt.Errorf("connectors: unrecognized provider %q", providerName)
	}

	conn, err := v.Connections.Load(ctx, tenantID, userID, providerName)
	if err != nil {
		return nil, err
	}
	aad := aadLabel(userID, providerName)
	access, err := crypto.Open(conn.DEK, conn.SealedAccess, tenantID.String(), aad)
	if err != nil {
		return nil, fmt.Errorf("open access token: %w", err)
	}
	tok := &oauth2.Token{AccessToken: string(access)}
	if len(conn.SealedRefresh) > 0 {
		refresh, err := crypto.Open(conn.DEK, conn.SealedRefresh, tenantID.String(), aad)
		if err != nil {
			return nil, fmt.Errorf("open refresh token: %w", err)
		}
		tok.RefreshToken = string(refresh)
	}
	if conn.ExpiresAt != nil {
		tok.Expiry = *conn.ExpiresAt
	}

	if !tok.Valid() && tok.RefreshToken != "" {
		fresh, err := oc.TokenSource(ctx, tok).Token()
		if err != nil {
			return nil, fmt.Errorf("connectors: refresh token: %w", err)
		}
		if fresh.AccessToken != tok.AccessToken {
			if err := v.persist(ctx, tenantID, userID, providerName, fresh); err != nil {
				return nil, fmt.Errorf("connectors: persist refreshed token: %w", err)
			}
		}
		return fresh, nil
	}
	return tok, nil
}

// AccessToken is Token's narrow projection for a caller (a connector Tool)
// that only ever needs the bearer value for an outbound Authorization
// header, never the refresh token or expiry — the smallest interface a
// builtin tool can depend on without importing golang.org/x/oauth2 itself.
func (v *Vault) AccessToken(ctx context.Context, tenantID, userID uuid.UUID, providerName string) (string, error) {
	tok, err := v.Token(ctx, tenantID, userID, providerName)
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}
