package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
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
// --public-key-out writes the verifying half of that key to a file, which is what
// the server is given (NEXUS_AUTHN_PUBLIC_KEY_PATH) instead of the signing key; this
// command, run wherever the signing key is kept, is the only thing that reads it.
// A missing --user mints a fresh random user id — fine for a demo/dev
// principal; a real per-user identity still comes from whatever a future
// OIDC provider's own login flow issues.
func runToken(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	tenantName := fs.String("tenant", "", "tenant name to mint a token for (required)")
	userArg := fs.String("user", "", "user id (uuid); a fresh random one is minted if omitted")
	ttl := fs.Duration("ttl", 24*time.Hour, "token validity duration")
	pubOut := fs.String("public-key-out", "", "write the verifying (public) key to this file, for the server's NEXUS_AUTHN_PUBLIC_KEY_PATH; with no --tenant, only that")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenantName == "" && *pubOut == "" {
		return errors.New("--tenant is required")
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
	if *pubOut != "" {
		// A public key is not secret: the server and anything that checks a token hold it.
		if err := os.WriteFile(*pubOut, authn.PublicKey(signingKey), 0o644); err != nil { //nolint:gosec // a public key, readable by the server's user on purpose
			return fmt.Errorf("write --public-key-out %s: %w", *pubOut, err)
		}
		fmt.Printf("public key written to %s\n", *pubOut)
		if *tenantName == "" {
			return nil
		}
	}
	issuer := authn.NewDevIssuer(signingKey)
	tok, err := issuer.Issue(tenantID, userID, *ttl)
	if err != nil {
		return fmt.Errorf("issue token: %w", err)
	}
	fmt.Printf("tenant_id: %s\nuser_id:   %s\ntoken:     %s\n", tenantID, userID, tok)
	return nil
}
