// Package signerkey holds the audit-chain Ed25519 private key and the only
// code in this module allowed to sign with it. It is imported ONLY by
// cmd/signerd (README.md §5, task 5.1: "nexusd can sign but cannot read the
// key") — tests/contract/boundaries_test.go's "nexusd must not import
// internal/audit/signerkey" rule makes that a structural property rather
// than a convention: cmd/nexusd has no way to compile against this package
// and reach the private key, only internal/audit.SignerClient's unix-socket
// RPC to cmd/signerd (which alone imports this package).
package signerkey

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Key is a loaded or freshly generated Ed25519 signing key, identified by
// KeyID so a receipt or anchor row can name which key produced its
// signature (rotation is a config change: a new Key with a new KeyID,
// never an in-place mutation of an existing one).
type Key struct {
	KeyID   string
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
}

// Sign signs digest directly — Ed25519 does its own hashing internally, so
// callers pass the already-computed chain/anchor hash (32 bytes), never
// raw content.
func (k Key) Sign(digest []byte) []byte {
	return ed25519.Sign(k.Private, digest)
}

// Generate creates a fresh random Ed25519 keypair under keyID.
func Generate(keyID string) (Key, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Key{}, fmt.Errorf("signerkey: generate: %w", err)
	}
	return Key{KeyID: keyID, Private: priv, Public: pub}, nil
}

// LoadOrGenerate reads a private key from path (raw 64-byte Ed25519 seed+
// public form) if it exists, or generates and persists a fresh one — the
// same "generate on first run" pattern cmd/nexusd's loadOrGenerateKEK
// already uses for the KEK, so `make up && make run` needs zero setup here
// either. keyID defaults to the file's base name when generating, so a
// signerd restarted against the same path keeps signing under the same
// identity every verifier already trusts.
func LoadOrGenerate(path string) (Key, error) {
	if raw, err := os.ReadFile(path); err == nil { //nolint:gosec // path is an operator-controlled config value, never request input
		if len(raw) != ed25519.PrivateKeySize {
			return Key{}, fmt.Errorf("signerkey: %s: want %d bytes, got %d", path, ed25519.PrivateKeySize, len(raw))
		}
		priv := ed25519.PrivateKey(raw)
		pub, ok := priv.Public().(ed25519.PublicKey)
		if !ok {
			return Key{}, fmt.Errorf("signerkey: %s: unexpected public key type", path)
		}
		return Key{KeyID: filepath.Base(path), Private: priv, Public: pub}, nil
	} else if !os.IsNotExist(err) {
		return Key{}, fmt.Errorf("signerkey: open %s: %w", path, err)
	}

	k, err := Generate(filepath.Base(path))
	if err != nil {
		return Key{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Key{}, fmt.Errorf("signerkey: create dir for %s: %w", path, err)
	}
	if err := writeKeyFile(path, k.Private); err != nil {
		return Key{}, err
	}
	return k, nil
}

func writeKeyFile(path string, priv ed25519.PrivateKey) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // path is an operator-controlled config value (NEXUS_SIGNER_KEY_PATH), never request input
	if err != nil {
		return fmt.Errorf("signerkey: create %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // best-effort close after an explicit Sync/Write error check below
	if _, err := io.Copy(f, bytes.NewReader(priv)); err != nil {
		return fmt.Errorf("signerkey: write %s: %w", path, err)
	}
	return nil
}
