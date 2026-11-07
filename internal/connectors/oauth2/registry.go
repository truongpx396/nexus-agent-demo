// Package oauth2 is the adapter that speaks the OAuth2 authorization-code
// protocol for internal/connectors (connectors.Authorizer), using
// golang.org/x/oauth2. It is the only place in the connector feature that
// links the protocol library and, through it, an HTTP client; the vault itself
// decides who may connect which provider and seals the tokens, and does neither
// here.
package oauth2

import (
	"context"
	"fmt"

	xoauth2 "golang.org/x/oauth2"

	"github.com/truongpx396/nexus-agent-demo/internal/connectors"
)

// Provider is one OAuth2 authorization-code provider this DEPLOYMENT
// recognizes — a small, operator-configured, process-wide registry
// (cmd/nexusd wires it from env vars), never a runtime user-editable list.
type Provider struct {
	Name         string
	Endpoint     xoauth2.Endpoint
	ClientID     string
	ClientSecret string
	Scopes       []string
	RedirectURL  string
}

// Registry is the static set of recognized providers, and the
// connectors.Authorizer over them.
type Registry struct {
	providers map[string]Provider
}

var _ connectors.Authorizer = (*Registry)(nil)

// NewRegistry returns a Registry of the given providers.
func NewRegistry(providers ...Provider) *Registry {
	m := make(map[string]Provider, len(providers))
	for _, p := range providers {
		m[p.Name] = p
	}
	return &Registry{providers: m}
}

func (r *Registry) config(name string) (xoauth2.Config, bool) {
	p, ok := r.providers[name]
	if !ok {
		return xoauth2.Config{}, false
	}
	return xoauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		Endpoint:     p.Endpoint,
		Scopes:       p.Scopes,
		RedirectURL:  p.RedirectURL,
	}, true
}

// Known implements connectors.Authorizer.
func (r *Registry) Known(provider string) bool {
	_, ok := r.providers[provider]
	return ok
}

// AuthCodeURL implements connectors.Authorizer.
func (r *Registry) AuthCodeURL(provider, state string) (string, error) {
	oc, ok := r.config(provider)
	if !ok {
		return "", fmt.Errorf("%w %q", connectors.ErrUnknownProvider, provider)
	}
	return oc.AuthCodeURL(state), nil
}

// Exchange implements connectors.Authorizer.
func (r *Registry) Exchange(ctx context.Context, provider, code string) (connectors.Token, error) {
	oc, ok := r.config(provider)
	if !ok {
		return connectors.Token{}, fmt.Errorf("%w %q", connectors.ErrUnknownProvider, provider)
	}
	tok, err := oc.Exchange(ctx, code)
	if err != nil {
		return connectors.Token{}, err
	}
	return fromOAuth2(tok), nil
}

// Refresh implements connectors.Authorizer.
func (r *Registry) Refresh(ctx context.Context, provider string, stale connectors.Token) (connectors.Token, error) {
	oc, ok := r.config(provider)
	if !ok {
		return connectors.Token{}, fmt.Errorf("%w %q", connectors.ErrUnknownProvider, provider)
	}
	fresh, err := oc.TokenSource(ctx, &xoauth2.Token{
		AccessToken: stale.AccessToken, RefreshToken: stale.RefreshToken, Expiry: stale.Expiry,
	}).Token()
	if err != nil {
		return connectors.Token{}, err
	}
	return fromOAuth2(fresh), nil
}

func fromOAuth2(t *xoauth2.Token) connectors.Token {
	return connectors.Token{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, Expiry: t.Expiry}
}
