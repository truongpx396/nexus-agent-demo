package main

import (
	"crypto/ed25519"
	"fmt"
	"os"

	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/authn"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
)

// defaultKEKPath is a local, gitignored dev key (see .gitignore's
// /.dev/ entry) — a production deployment sources the KEK from an external
// vault/HSM behind the same internal/crypto.KEK type (internal/crypto's doc
// comment). Generated on first run so `make up && make run` works with zero
// setup.
const defaultKEKPath = ".dev/kek.key"

// loadOrGenerateKEK loads the KEK from path. Outside dev mode (README task
// 13.11/F12), a missing file is fatal — a deploy that forgot to mount its
// KEK must never start "successfully" holding a brand-new key that can't
// unwrap any existing tenant's DEK. Only inside dev mode does a missing file
// bootstrap a fresh one, exactly as this always behaved before F12.
func loadOrGenerateKEK(path string, dev bool) (crypto.KEK, error) {
	f, err := os.Open(path) //nolint:gosec // path is an operator-controlled config value (NEXUS_KEK_PATH), never request input
	if err == nil {
		defer f.Close() //nolint:errcheck // read-only handle; nothing to flush
		return crypto.LoadKEK(f)
	}
	if !os.IsNotExist(err) {
		return crypto.KEK{}, fmt.Errorf("open KEK file %s: %w", path, err)
	}
	if !dev {
		return crypto.KEK{}, fmt.Errorf("KEK file %s does not exist (pass --dev to auto-generate one for local development; a production deployment must source it from a real vault/HSM)", path)
	}

	kek, err := crypto.GenerateKEK()
	if err != nil {
		return crypto.KEK{}, fmt.Errorf("generate KEK: %w", err)
	}
	if err := os.MkdirAll(".dev", 0o700); err != nil {
		return crypto.KEK{}, fmt.Errorf("create .dev: %w", err)
	}
	if err := os.WriteFile(path, kek.Bytes(), 0o600); err != nil {
		return crypto.KEK{}, fmt.Errorf("write KEK file %s: %w", path, err)
	}
	log.Info().Str("path", path).Msg("generated a new dev KEK")
	return kek, nil
}

// defaultAuthnSigningKeyPath mirrors defaultKEKPath's own local-gitignored-
// dev-file convention (.gitignore's /.dev/ entry).
const defaultAuthnSigningKeyPath = ".dev/authn_signing.key"

// loadOrGenerateSigningKey loads the AuthN Ed25519 signing key from path,
// following loadOrGenerateKEK's identical load-or-generate-gated-by-dev
// shape (README task 13.1/13.11, F1/F12) — outside dev mode, a missing file
// is fatal rather than a silently minted key nothing else in the fleet
// would ever accept a token from.
func loadOrGenerateSigningKey(path string, dev bool) (ed25519.PrivateKey, error) {
	f, err := os.Open(path) //nolint:gosec // path is an operator-controlled config value (NEXUS_AUTHN_SIGNING_KEY_PATH), never request input
	if err == nil {
		defer f.Close() //nolint:errcheck // read-only handle; nothing to flush
		return authn.LoadPrivateKey(f)
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("open AuthN signing key file %s: %w", path, err)
	}
	if !dev {
		return nil, fmt.Errorf("AuthN signing key file %s does not exist (pass --dev to auto-generate one for local development)", path)
	}

	key, err := authn.GeneratePrivateKey()
	if err != nil {
		return nil, fmt.Errorf("generate AuthN signing key: %w", err)
	}
	if err := os.MkdirAll(".dev", 0o700); err != nil {
		return nil, fmt.Errorf("create .dev: %w", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("write AuthN signing key file %s: %w", path, err)
	}
	log.Info().Str("path", path).Msg("generated a new dev AuthN signing key")
	return key, nil
}
