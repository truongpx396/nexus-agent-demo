package connectors

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
)

// StateStore holds the authorization-code flow's CSRF state between BeginAuth
// and HandleCallback. It is deliberately short-lived (minutes, not the life of
// a connection) and needs no audit durability, which is why it is not a
// table. The Redis implementation is internal/connectors/redis.
type StateStore interface {
	// Put stores raw under state for ttl.
	Put(ctx context.Context, state string, raw []byte, ttl time.Duration) error

	// Take returns the value stored under state and removes it, atomically: of
	// two callers presenting the same state exactly one gets it, so a replayed
	// callback finds nothing and fails closed. found is false for a state that
	// is unknown, expired or already taken.
	Take(ctx context.Context, state string) (raw []byte, found bool, err error)
}

// StoredConnection is one saved OAuth connection: its tokens still sealed,
// with the tenant data key that seals them already unwrapped.
type StoredConnection struct {
	SealedAccess  []byte
	SealedRefresh []byte // nil when the provider issued no refresh token
	ExpiresAt     *time.Time
	DEK           crypto.DEK
}

// Connections persists the sealed tokens of each (tenant, user, provider)
// connection. The Postgres implementation is internal/connectors/postgres.
type Connections interface {
	// Save mints a fresh data key for tenantID, passes it to seal, and stores
	// the sealed access and refresh tokens seal returns (refresh may be nil),
	// replacing any earlier connection for the same user and provider. The key
	// and the connection are stored together or not at all, so a key is never
	// minted for a connection that was not saved.
	Save(ctx context.Context, tenantID, userID uuid.UUID, provider string, expiresAt *time.Time,
		seal func(dek crypto.DEK) (access, refresh []byte, err error)) error

	// Load returns the connection, or an error if there is none.
	Load(ctx context.Context, tenantID, userID uuid.UUID, provider string) (StoredConnection, error)
}
