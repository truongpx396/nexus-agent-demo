package mcp

import (
	"context"

	"github.com/google/uuid"
)

// Server is an MCP server a tenant has registered (migrations/0019_mcp_servers.sql),
// as the resolver sees it. The sealed credential and the key it is sealed
// under are the storage adapter's business and are deliberately not here.
type Server struct {
	ServerID      uuid.UUID
	Name          string
	BaseURL       string
	AuthKind      string // "bearer_static", "oauth_connector" or "none"
	OAuthProvider string // set when AuthKind is "oauth_connector"
	Status        string // "admitted" is the only status that resolves
}

// ServerDirectory is the resolver's storage. Every method runs in the named
// tenant's own scope; the Postgres adapter is internal/surfaces/mcp/postgres.
type ServerDirectory interface {
	// Admitted lists the tenant's admitted servers by name — the set the
	// per-run catalog (port.go) is built from.
	Admitted(ctx context.Context, tenantID uuid.UUID) ([]Server, error)

	// Find loads one server by name, whatever its status; found=false means
	// no such server.
	Find(ctx context.Context, tenantID uuid.UUID, name string) (srv Server, found bool, err error)

	// StaticToken returns the plaintext bearer token of a bearer_static
	// server. It exists so the credential is opened where its key lives and
	// never travels as ciphertext through the resolver.
	StaticToken(ctx context.Context, tenantID uuid.UUID, serverName string) (string, error)

	// SessionUser returns the user a session runs as. An oauth_connector
	// server authenticates as that user, never as a service account.
	SessionUser(ctx context.Context, tenantID, sessionID uuid.UUID) (uuid.UUID, error)
}
