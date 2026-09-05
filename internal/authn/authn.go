// Package authn resolves an inbound request's bearer token to the tenant/
// user it authenticates (README.md task 13.1, closing production-readiness
// finding F1: the calling principal used to be read straight off an
// unverified X-Nexus-Tenant-ID/X-Nexus-User-ID header pair).
//
// Verifier is the seam every caller (internal/surfaces/rest,
// internal/surfaces/cli) depends on — never a concrete key type — so a real
// per-tenant OIDC provider (Casdoor, Keycloak, Auth0, or any other IdP) is a
// second implementation of the same interface later, wired in only from
// cmd/nexusd, with zero change to either surface. DevIssuer/DevVerifier
// (Ed25519, a local key file) are the only implementation this phase ships.
package authn

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Verifier resolves a bearer token string to the (tenantID, userID) pair it
// authenticates, or an error if the token is missing, malformed, expired, or
// fails signature verification. Implementations must fail closed: any error
// return means "not authenticated," never a zero-value principal treated as
// valid.
type Verifier interface {
	Verify(tokenString string) (tenantID, userID uuid.UUID, err error)
}

// issuer is the fixed `iss` claim DevIssuer stamps and DevVerifier requires —
// binds a token to this specific dev key pair rather than accepting any
// well-formed EdDSA JWT.
const issuer = "nexusd-dev-issuer"

// claims is the JWT payload DevIssuer mints and DevVerifier checks. Private
// claim names (tenant_id/user_id) are fine for a dev-only issuer; a future
// OIDC Verifier implementation maps whatever claims its own IdP uses
// internally instead — this shape is not a public contract.
type claims struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	jwt.RegisteredClaims
}

// DevIssuer mints tokens. Split from DevVerifier (rather than one type
// holding both halves of the key) because a real deployment runs the
// verifier (public key only) inside nexusd's request path but the issuer
// (private key) only inside an operator's own `nexusd token` invocation.
type DevIssuer struct {
	key ed25519.PrivateKey
}

func NewDevIssuer(key ed25519.PrivateKey) *DevIssuer { return &DevIssuer{key: key} }

// Issue mints a token valid for ttl, authenticating tenantID/userID.
func (i *DevIssuer) Issue(tenantID, userID uuid.UUID, ttl time.Duration) (string, error) {
	now := time.Now()
	c := claims{
		TenantID: tenantID.String(),
		UserID:   userID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	signed, err := tok.SignedString(i.key)
	if err != nil {
		return "", fmt.Errorf("authn: sign token: %w", err)
	}
	return signed, nil
}

// DevVerifier checks tokens DevIssuer minted, against the public half of the
// same key pair. Implements Verifier.
type DevVerifier struct {
	key ed25519.PublicKey
}

func NewDevVerifier(key ed25519.PublicKey) *DevVerifier { return &DevVerifier{key: key} }

func (v *DevVerifier) Verify(tokenString string) (tenantID, userID uuid.UUID, err error) {
	var c claims
	_, err = jwt.ParseWithClaims(tokenString, &c, func(t *jwt.Token) (any, error) {
		return v.key, nil
	}, jwt.WithIssuer(issuer), jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithExpirationRequired())
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("authn: verify token: %w", err)
	}
	tenantID, err = uuid.Parse(c.TenantID)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("authn: invalid tenant_id claim: %w", err)
	}
	userID, err = uuid.Parse(c.UserID)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("authn: invalid user_id claim: %w", err)
	}
	return tenantID, userID, nil
}

// --- signing-key file I/O ---
//
// Mirrors internal/crypto.KEK's LoadKEK/GenerateKEK/Bytes convention exactly
// (raw key bytes in a local file), so cmd/nexusd's own key-loading helper
// follows the identical load-or-generate-gated-by-dev-mode shape for both
// keys (production-readiness finding F12).

// LoadPrivateKey reads exactly ed25519.PrivateKeySize raw bytes from r as
// the signing key.
func LoadPrivateKey(r io.Reader) (ed25519.PrivateKey, error) {
	buf := make([]byte, ed25519.PrivateKeySize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("authn: read signing key: %w", err)
	}
	return ed25519.PrivateKey(buf), nil
}

// GeneratePrivateKey creates a fresh random Ed25519 key pair — used only to
// bootstrap a local dev environment's key file, gated by devMode at every
// call site (cmd/nexusd), never called on a path a verifier already depends
// on.
func GeneratePrivateKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("authn: generate signing key: %w", err)
	}
	return priv, nil
}

// PublicKey extracts the verifying half from a loaded/generated private key.
func PublicKey(priv ed25519.PrivateKey) ed25519.PublicKey {
	return priv.Public().(ed25519.PublicKey) //nolint:forcetypeassert // ed25519.PrivateKey.Public() always returns ed25519.PublicKey
}
