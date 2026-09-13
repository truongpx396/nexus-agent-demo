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
	DatabaseURL         string
	RedisAddr           string
	SignerdSocket       string
	KEKPath             string
	AuthnSigningKeyPath string
	HTTPAddr            string
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
			HTTPAddr:            envOr("NEXUS_HTTP_ADDR", ":8055"),
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
		DatabaseURL:         require("NEXUS_DATABASE_URL"),
		RedisAddr:           require("NEXUS_REDIS_ADDR"),
		SignerdSocket:       require("NEXUS_SIGNERD_SOCKET"),
		KEKPath:             require("NEXUS_KEK_PATH"),
		AuthnSigningKeyPath: require("NEXUS_AUTHN_SIGNING_KEY_PATH"),
		HTTPAddr:            envOr("NEXUS_HTTP_ADDR", ":8080"), // a listen address has no unsafe-default failure mode; --dev doesn't change what this defaults to
	}
	if len(missing) > 0 {
		return serverConfig{}, fmt.Errorf(
			"missing required environment variable(s) outside --dev mode: %s (pass --dev for local development defaults)",
			strings.Join(missing, ", "),
		)
	}
	return cfg, nil
}
