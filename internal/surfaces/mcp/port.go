package mcp

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// Port is internal/surfaces/rest.Server's optional MCP seam — the same
// nil-valid-optional-interface idiom rest.Server.Skills (SkillSetPort)
// already uses (rest/server.go). handleCreateRun calls Resolve ONCE per run
// to get both the extra model-visible catalog entries
// (RunRequest.ExtraCatalog) and the digest folded into
// harness.Config.MCPCatalogDigest — a single call, not two, so both are
// always computed from the exact same admitted-server listing resolved as
// the exact same user; Digest and Catalog as two separate calls would risk
// silently disagreeing (e.g. an oauth_connector-authed server resolvable
// for the catalog but invisible to a digest computed under a different
// user).
type Port struct {
	Resolver *Resolver
}

// entries lists every tool every one of tenantID's admitted servers
// currently offers, each already qualified as its own real ToolRef string
// (mcp/{server}/{tool}@{version}) — the exact form provider.ToolSchema.Name
// already uses for every other tool in this codebase (cmd/nexusd's own
// newToolPipeline: `Name: d.ID.String()`). Resolved as userID so an
// oauth_connector-authed server authenticates as the calling user, never a
// service account (README task 11's own point).
func (p *Port) entries(ctx context.Context, tenantID, userID uuid.UUID) ([]provider.ToolSchema, error) {
	var servers []serverRow
	err := p.Resolver.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		servers, err = listAdmittedServers(ctx, tx, tenantID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: list admitted servers: %w", err)
	}

	var out []provider.ToolSchema
	for _, server := range servers {
		var bearer string
		err := p.Resolver.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			bearer, err = p.Resolver.resolveAuth(ctx, tx, tenantID, userID, server)
			return err
		})
		if err != nil {
			// One misconfigured/unreachable server must not take down a
			// run's whole catalog — skip it and keep going, the same
			// "denied at layer 4, not a process crash" honesty README's own
			// Phase 11 demo language expects of a refused MCP tool.
			continue
		}
		client := &Client{BaseURL: server.BaseURL, BearerToken: bearer, HTTPClient: p.Resolver.Client}
		list, err := p.Resolver.listTools(ctx, server, client)
		if err != nil {
			continue
		}
		for _, t := range list {
			ref := "mcp/" + server.Name + "/" + t.Name + "@v" + t.SchemaDigest()
			out = append(out, provider.ToolSchema{Name: ref, Description: t.Description, InputSchema: t.InputSchema})
		}
	}
	return out, nil
}

// Resolve returns tenantID's current MCP tool set, resolved as userID, both
// as the extra model-visible catalog entries one run's RunRequest carries
// and as a stable digest for harness.Config.MCPCatalogDigest.
func (p *Port) Resolve(ctx context.Context, tenantID, userID uuid.UUID) (schemas []provider.ToolSchema, digest []byte, err error) {
	entries, err := p.entries(ctx, tenantID, userID)
	if err != nil {
		return nil, nil, err
	}
	h := sha256.New()
	for _, e := range entries {
		_, _ = fmt.Fprintf(h, "%s:%x\n", e.Name, sha256.Sum256(e.InputSchema))
	}
	return entries, h.Sum(nil), nil
}
