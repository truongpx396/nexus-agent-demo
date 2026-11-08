package connectors

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
)

// Token is an OAuth token pair as the vault handles it: the bearer value, the
// refresh credential if the provider issued one, and when the bearer expires.
// It is the vault's own type so the package's API does not carry the OAuth
// protocol library's; the adapter (internal/connectors/oauth2) converts.
type Token struct {
	AccessToken  string
	RefreshToken string
	Expiry       time.Time // zero means the provider gave no expiry
}

// expiryDelta is how early a token counts as expired, so one that would lapse
// in flight is refreshed first. It matches the protocol library's own margin.
const expiryDelta = 10 * time.Second

// Valid reports whether the token has a bearer value and is not about to expire.
func (t Token) Valid() bool {
	return t.AccessToken != "" && (t.Expiry.IsZero() || !t.Expiry.Round(0).Add(-expiryDelta).Before(time.Now()))
}

// ErrUnknownProvider is returned for a provider the deployment does not
// recognise. It is distinct from a provider a tenant has not admitted: both fail
// closed, and they are different mistakes.
var ErrUnknownProvider = errors.New("connectors: unrecognized provider")

// Authorizer is the OAuth2 authorization-code protocol, for the providers this
// deployment recognises: the consent URL, the code exchange and the refresh. The
// vault decides who may connect what and seals the result; this is only the
// conversation with the provider. The implementation is
// internal/connectors/oauth2.
type Authorizer interface {
	// Known reports whether the deployment recognises provider.
	Known(provider string) bool

	// AuthCodeURL returns the provider's consent URL carrying state, or an error
	// wrapping ErrUnknownProvider.
	AuthCodeURL(provider, state string) (string, error)

	// Exchange trades an authorization code for tokens.
	Exchange(ctx context.Context, provider, code string) (Token, error)

	// Refresh returns a fresh token for stale, which carries a refresh token.
	Refresh(ctx context.Context, provider string, stale Token) (Token, error)
}

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
