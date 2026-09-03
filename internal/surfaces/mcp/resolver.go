package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// TokenSource is the seam into internal/connectors.Vault this package
// depends on structurally rather than by import (the same decoupling idiom
// tools.SandboxExec already uses for internal/sandbox) — used only when a
// server's auth_kind is 'oauth_connector'.
type TokenSource interface {
	AccessToken(ctx context.Context, tenantID, userID uuid.UUID, provider string) (string, error)
}

// serverRow mirrors one migrations/0019_mcp_servers.sql row.
type serverRow struct {
	ServerID          uuid.UUID
	Name              string
	BaseURL           string
	AuthKind          string
	SealedStaticToken []byte
	KeyID             string
	OAuthProvider     string
	Status            string
}

// listAdmittedServers returns every server tenantID has admitted — the set
// port.go's Catalog/Digest enumerate to build the tenant's per-run MCP
// contribution to the model-visible catalog.
func listAdmittedServers(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]serverRow, error) {
	rows, err := tx.Query(ctx,
		`SELECT server_id, name, base_url, auth_kind, sealed_static_token, key_id, oauth_provider, status
		 FROM mcp_servers WHERE tenant_id = $1 AND status = 'admitted' ORDER BY name`,
		tenantID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []serverRow
	for rows.Next() {
		var r serverRow
		var keyID, oauthProvider *string
		if err := rows.Scan(&r.ServerID, &r.Name, &r.BaseURL, &r.AuthKind, &r.SealedStaticToken, &keyID, &oauthProvider, &r.Status); err != nil {
			return nil, err
		}
		if keyID != nil {
			r.KeyID = *keyID
		}
		if oauthProvider != nil {
			r.OAuthProvider = *oauthProvider
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func loadServerRow(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, name string) (serverRow, bool, error) {
	var r serverRow
	var keyID, oauthProvider *string
	err := tx.QueryRow(ctx,
		`SELECT server_id, name, base_url, auth_kind, sealed_static_token, key_id, oauth_provider, status
		 FROM mcp_servers WHERE tenant_id = $1 AND name = $2`,
		tenantID, name,
	).Scan(&r.ServerID, &r.Name, &r.BaseURL, &r.AuthKind, &r.SealedStaticToken, &keyID, &oauthProvider, &r.Status)
	if err != nil {
		if err == pgx.ErrNoRows {
			return serverRow{}, false, nil
		}
		return serverRow{}, false, err
	}
	if keyID != nil {
		r.KeyID = *keyID
	}
	if oauthProvider != nil {
		r.OAuthProvider = *oauthProvider
	}
	return r, true, nil
}

type cachedListing struct {
	tools     []Tool
	fetchedAt time.Time
}

// Resolver implements tools.DynamicResolver for Phase 11's per-tenant MCP
// tools (README task 11.1): a ref the static, process-wide Manifest/Registry
// don't know about falls through to Resolve, which loads the tenant's own
// admitted mcp_servers row, lists that server's current tools, and — only
// if the tool's live schema digest still matches what the qualified ref's
// own Version encodes — wraps it as an ordinary tools.Tool. Every failure
// mode (unadmitted server, disabled status, unknown tool, drifted schema)
// collapses to ok=false: the pipeline's own unknown_tool error, never a
// distinguishable "exists but refused" signal (resolveTool's own doc
// comment in internal/tools/pipeline.go).
type Resolver struct {
	Store  *store.Store
	Keys   *crypto.KeyStore
	Tokens TokenSource
	Client *http.Client

	// AllowedHosts is the SAME egress allowlist README task 5.13 already
	// established for internal/tools/builtin.WebFetch, extended per task
	// 11.9 to cover a newly admitted MCP server's own base_url host —
	// reusing hostAllowed's fail-closed semantics (nil/empty refuses every
	// host) rather than a second allowlist mechanism. cmd/nexusd wires this
	// from the SAME NEXUS_WEB_FETCH_ALLOWLIST env var WebFetch/
	// ConnectorFetch already read.
	AllowedHosts []string

	// CacheTTL bounds how long a server's tools/list result is reused
	// before Resolve re-fetches it — avoids a network round trip on every
	// single tool call while still catching a schema change reasonably
	// promptly. Zero uses a 60-second default.
	CacheTTL time.Duration

	mu    sync.Mutex
	cache map[uuid.UUID]cachedListing
}

// hostAllowed is internal/tools/builtin's own function of the same name,
// duplicated per this codebase's established cross-package idiom (outbox.go's
// own doc comment) rather than imported — this package has no other reason
// to depend on internal/tools/builtin.
func hostAllowed(patterns []string, host string) bool {
	for _, p := range patterns {
		switch {
		case p == "*":
			return true
		case p == host:
			return true
		case strings.HasPrefix(p, "*."):
			if suffix := strings.TrimPrefix(p, "*"); strings.HasSuffix(host, suffix) {
				return true
			}
		}
	}
	return false
}

func (r *Resolver) cacheTTL() time.Duration {
	if r.CacheTTL <= 0 {
		return 60 * time.Second
	}
	return r.CacheTTL
}

func (r *Resolver) listTools(ctx context.Context, server serverRow, client *Client) ([]Tool, error) {
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[uuid.UUID]cachedListing{}
	}
	if c, ok := r.cache[server.ServerID]; ok && time.Since(c.fetchedAt) < r.cacheTTL() {
		r.mu.Unlock()
		return c.tools, nil
	}
	r.mu.Unlock()

	u, err := url.Parse(server.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("%s has an invalid base_url: %w", server.Name, err)
	}
	if !hostAllowed(r.AllowedHosts, u.Hostname()) {
		return nil, fmt.Errorf("egress_denied: %s's base_url host %q is not on the allowlist", server.Name, u.Hostname())
	}

	if err := client.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("initialize %s: %w", server.Name, err)
	}
	list, err := client.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tools for %s: %w", server.Name, err)
	}

	r.mu.Lock()
	r.cache[server.ServerID] = cachedListing{tools: list, fetchedAt: time.Now()}
	r.mu.Unlock()
	return list, nil
}

// resolveAuth resolves the bearer credential (if any) for one call/listing
// against server, given the user to authenticate as when auth_kind is
// 'oauth_connector' — userID is the zero UUID for every other auth_kind and
// simply goes unused. Factored out of Resolve so port.go's Catalog (which
// has userID directly from the REST surface's own resolved Principal, with
// no session row to look one up from — session creation hasn't happened
// yet at catalog-build time) can resolve the identical credential without
// duplicating the switch.
func (r *Resolver) resolveAuth(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, server serverRow) (string, error) {
	switch server.AuthKind {
	case "bearer_static":
		if server.KeyID == "" {
			return "", fmt.Errorf("server %q declares bearer_static auth with no key_id", server.Name)
		}
		dek, err := r.Keys.Unwrap(ctx, tx, server.KeyID)
		if err != nil {
			return "", fmt.Errorf("unwrap static token key: %w", err)
		}
		aad := tenantID.String() + "|mcp|" + server.Name
		raw, err := crypto.Open(dek, server.SealedStaticToken, tenantID.String(), aad)
		if err != nil {
			return "", fmt.Errorf("open static token: %w", err)
		}
		return string(raw), nil
	case "oauth_connector":
		if r.Tokens == nil {
			return "", fmt.Errorf("server %q requires oauth_connector auth but no TokenSource is configured", server.Name)
		}
		tok, err := r.Tokens.AccessToken(ctx, tenantID, userID, server.OAuthProvider)
		if err != nil {
			return "", fmt.Errorf("resolve oauth token for provider %q: %w", server.OAuthProvider, err)
		}
		return tok, nil
	case "none":
		return "", nil
	default:
		return "", fmt.Errorf("server %q has an unrecognized auth_kind %q", server.Name, server.AuthKind)
	}
}

// Resolve implements tools.DynamicResolver.
func (r *Resolver) Resolve(ctx context.Context, tenantID, sessionID uuid.UUID, ref tools.ToolRef) (tools.Tool, bool, error) {
	name, ok := strings.CutPrefix(ref.Namespace, "mcp/")
	if !ok || name == "" {
		return nil, false, nil // not an MCP-qualified ref at all
	}

	var server serverRow
	var bearer string
	err := r.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var found bool
		var err error
		server, found, err = loadServerRow(ctx, tx, tenantID, name)
		if err != nil {
			return fmt.Errorf("load mcp_servers row: %w", err)
		}
		if !found || server.Status != "admitted" {
			server = serverRow{} // signal "not resolvable" below without a distinguishable reason
			return nil
		}

		var userID uuid.UUID
		if server.AuthKind == "oauth_connector" {
			sess, err := store.GetSession(ctx, tx, sessionID)
			if err != nil {
				return fmt.Errorf("resolve calling session: %w", err)
			}
			userID = sess.UserID
		}
		bearer, err = r.resolveAuth(ctx, tx, tenantID, userID, server)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if server.ServerID == uuid.Nil {
		return nil, false, nil // unadmitted, disabled, or unknown server — fail closed, indistinguishable from non-existence
	}

	client := &Client{BaseURL: server.BaseURL, BearerToken: bearer, HTTPClient: r.Client}
	list, err := r.listTools(ctx, server, client)
	if err != nil {
		return nil, false, err
	}

	for _, t := range list {
		if t.Name != ref.Name {
			continue
		}
		wantVersion := "v" + t.SchemaDigest()
		if wantVersion != ref.Version {
			// The tool exists, but its live schema no longer matches what
			// this qualified ref pins (README task 11.1's #15 "digest
			// re-verification at use") — a schema change is a fresh
			// admission decision, not a drifted one, so this specific ref
			// is simply not resolvable any more.
			return nil, false, nil
		}
		return &adapter{
			ref:    tools.ToolRef{Namespace: ref.Namespace, Name: t.Name, Version: wantVersion},
			remote: t,
			client: client,
		}, true, nil
	}
	return nil, false, nil
}
