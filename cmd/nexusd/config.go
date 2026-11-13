package main

import (
	"fmt"
	"os"
	"strings"
)

// serverConfig is the security/connectivity-critical settings serve() needs
// (README task 14.4, closing production-readiness finding F12) — a single
// validated struct read once at startup, rather than the ~35 scattered
// envOr calls the rest of this file's admin subcommands still use. Scoped
// to serve() specifically: F12's own example ("a deploy that forgets
// NEXUS_DATABASE_URL starts cleanly and dials a local default") is about the
// long-running network-facing process, not a one-shot operator command like
// `nexusd seed` or `nexusd dashboard` — those keep their existing envOr
// defaults.
type serverConfig struct {
	DatabaseURL   string
	RedisAddr     string
	SignerdSocket string
	KEKPath       string
	HTTPAddr      string

	// AuthnPublicKeyPath is the Ed25519 public key bearer tokens are verified with.
	// The server only verifies, so outside --dev it is given this and never the
	// signing key, whose file would let anyone who can read it mint a token for any
	// tenant. Under --dev, when it is unset, the verifying key is derived from
	// AuthnSigningKeyPath, which is where `nexusd --dev token` mints from.
	AuthnPublicKeyPath string
	// AuthnSigningKeyPath is only read under --dev (see AuthnPublicKeyPath).
	AuthnSigningKeyPath string

	// AdminDatabaseURL is the superuser DSN the few cross-tenant operations use (the
	// tenant directory, the orphan scan, the queue workers' admission). adminDSN
	// reads it from the environment and falls back to a local default, which is
	// right for --dev and wrong anywhere else: a deployment that forgot it would
	// start and then fail those loops quietly against localhost.
	AdminDatabaseURL string
	// MetricsToken is the bearer token /metrics requires. It lists every tenant's
	// id, spend and tool names, so it is not served to whoever can reach the port.
	// Empty only under --dev.
	MetricsToken string
}

// loadServerConfig reads serverConfig from the environment. In dev mode
// (--dev) every field falls back to today's zero-setup default, exactly the
// pre-14.4 behavior. Outside dev mode, every field is REQUIRED — a missing
// one is fatal, and every missing one is reported together in a single
// error (fail fast, not one surprise at a time) rather than starting
// "successfully" against a localhost default no production deployment
// actually intends.
func loadServerConfig(dev bool) (serverConfig, error) {
	if dev {
		return serverConfig{
			DatabaseURL:         envOr("NEXUS_DATABASE_URL", defaultAppDSN),
			RedisAddr:           envOr("NEXUS_REDIS_ADDR", defaultRedisAddr),
			SignerdSocket:       envOr("NEXUS_SIGNERD_SOCKET", defaultSignerdSocket),
			KEKPath:             envOr("NEXUS_KEK_PATH", defaultKEKPath),
			AuthnSigningKeyPath: envOr("NEXUS_AUTHN_SIGNING_KEY_PATH", defaultAuthnSigningKeyPath),
			AuthnPublicKeyPath:  os.Getenv("NEXUS_AUTHN_PUBLIC_KEY_PATH"), // optional under --dev: derived from the signing key when unset
			HTTPAddr:            envOr("NEXUS_HTTP_ADDR", ":8085"),
			AdminDatabaseURL:    adminDSN(),
			MetricsToken:        os.Getenv("NEXUS_METRICS_TOKEN"), // optional under --dev: /metrics stays open for the local scrape
		}, nil
	}

	var missing []string
	require := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}
	cfg := serverConfig{
		DatabaseURL:        require("NEXUS_DATABASE_URL"),
		RedisAddr:          require("NEXUS_REDIS_ADDR"),
		SignerdSocket:      require("NEXUS_SIGNERD_SOCKET"),
		KEKPath:            require("NEXUS_KEK_PATH"),
		AuthnPublicKeyPath: require("NEXUS_AUTHN_PUBLIC_KEY_PATH"),
		HTTPAddr:           envOr("NEXUS_HTTP_ADDR", ":8085"), // a listen address has no unsafe-default failure mode; --dev doesn't change what this defaults to
		AdminDatabaseURL:   require("NEXUS_ADMIN_DATABASE_URL"),
		MetricsToken:       require("NEXUS_METRICS_TOKEN"),
	}
	if len(missing) > 0 {
		return serverConfig{}, fmt.Errorf(
			"missing required environment variable(s) outside --dev mode: %s (pass --dev for local development defaults)",
			strings.Join(missing, ", "),
		)
	}
	return cfg, nil
}
