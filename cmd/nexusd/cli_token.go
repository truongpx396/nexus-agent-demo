package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/authn"
)

// runToken is `nexusd token --tenant=<name> [--user=<uuid>] [--ttl=1h]`
// (README task 13.1): mints a bearer token against the same AuthN signing
// key `serve()` verifies against, for a client (`nexusctl`, the web app,
// curl) to send as `Authorization: Bearer <token>`. --tenant is hashed into
// a deterministic tenant id the same way `nexusd seed`'s own --tenant is, so
// a token minted before or after seeding names the same tenant either way.
// A missing --user mints a fresh random user id — fine for a demo/dev
// principal; a real per-user identity still comes from whatever a future
// OIDC provider's own login flow issues.
func runToken(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	tenantName := fs.String("tenant", "", "tenant name to mint a token for (required)")
	userArg := fs.String("user", "", "user id (uuid); a fresh random one is minted if omitted")
	ttl := fs.Duration("ttl", 24*time.Hour, "token validity duration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenantName == "" {
		return fmt.Errorf("--tenant is required")
	}
	tenantID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("nexus-agent-demo/tenant/"+*tenantName))

	userID := uuid.New()
	if *userArg != "" {
		parsed, err := uuid.Parse(*userArg)
		if err != nil {
			return fmt.Errorf("invalid --user: %w", err)
		}
		userID = parsed
	}

	signingKey, err := loadOrGenerateSigningKey(envOr("NEXUS_AUTHN_SIGNING_KEY_PATH", defaultAuthnSigningKeyPath), devMode)
	if err != nil {
		return fmt.Errorf("load AuthN signing key: %w", err)
	}
	issuer := authn.NewDevIssuer(signingKey)
	tok, err := issuer.Issue(tenantID, userID, *ttl)
	if err != nil {
		return fmt.Errorf("issue token: %w", err)
	}
	fmt.Printf("tenant_id: %s\nuser_id:   %s\ntoken:     %s\n", tenantID, userID, tok)
	return nil
}
