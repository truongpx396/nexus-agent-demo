// Package crypto implements per-tenant envelope encryption: an operator-held
// key-encryption key (KEK) wraps a random per-tenant data-encryption key
// (DEK), and the DEK seals event payloads. Erasure (Phase 5) destroys the
// DEK, not the ciphertext — content becomes permanently unrecoverable while
// the event sequence, its digests, and the audit chain stay intact
// (docs/constitution.md, "Erasure reconciled with append-only").
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

const keySize = 32 // AES-256

// KEK is the operator-held key-encryption key. It never seals event payloads
// directly — it only wraps DEKs, so rotating it re-wraps keys rather than
// re-encrypting every event ever written.
type KEK struct {
	key []byte
}

// LoadKEK reads exactly 32 raw bytes from r as the KEK. In this demo the KEK
// lives in a local file (README.md's `.dev/` — gitignored); a production
// deployment would source it from an external vault/HSM instead, behind the
// same type.
func LoadKEK(r io.Reader) (KEK, error) {
	buf := make([]byte, keySize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return KEK{}, fmt.Errorf("read KEK: %w", err)
	}
	return KEK{key: buf}, nil
}

// GenerateKEK creates a fresh random KEK — used to bootstrap a local dev
// environment's `.dev/kek.key` file; never called on a path a tenant's data
// already depends on.
func GenerateKEK() (KEK, error) {
	buf := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return KEK{}, fmt.Errorf("generate KEK: %w", err)
	}
	return KEK{key: buf}, nil
}

func (k KEK) Bytes() []byte { return k.key }

// DEK is a per-tenant data-encryption key. KeyID is the value stored in
// Event.key_id; destroying the row that holds this key's wrapped form is
// erasure (FR-080).
type DEK struct {
	KeyID string
	Key   []byte
}

// GenerateDEK creates a fresh random 32-byte DEK. keyID is supplied by the
// caller (internal/crypto.KeyStore mints a uuid) rather than generated here,
// so KeyID always matches the encryption_keys.key_id row it is persisted
// under.
func GenerateDEK(keyID string) (DEK, error) {
	buf := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return DEK{}, fmt.Errorf("generate DEK: %w", err)
	}
	return DEK{KeyID: keyID, Key: buf}, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new GCM: %w", err)
	}
	return gcm, nil
}

// sealWithKey seals plaintext under key with a fresh random nonce, returning
// nonce||ciphertext. aad (additional authenticated data) is bound into the
// tag but not encrypted — callers pass identifiers (tenant_id, key_id) that
// must not be swappable between rows without detection.
func sealWithKey(key, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, aad)
	return append(nonce, ciphertext...), nil
}

func openWithKey(key, sealed, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("sealed value shorter than nonce")
	}
	nonce, ciphertext := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	return plaintext, nil
}

// WrapDEK seals a DEK's raw key bytes under the KEK, for storage in
// encryption_keys.wrapped_dek. keyID is bound as AAD so a wrapped DEK copied
// onto a different key_id row fails to open.
func WrapDEK(kek KEK, dek DEK) ([]byte, error) {
	return sealWithKey(kek.Bytes(), dek.Key, []byte(dek.KeyID))
}

// UnwrapDEK reverses WrapDEK, given the key_id the wrapped bytes were
// originally bound to.
func UnwrapDEK(kek KEK, keyID string, wrapped []byte) (DEK, error) {
	raw, err := openWithKey(kek.Bytes(), wrapped, []byte(keyID))
	if err != nil {
		return DEK{}, fmt.Errorf("unwrap DEK: %w", err)
	}
	return DEK{KeyID: keyID, Key: raw}, nil
}

// Seal encrypts an event payload under dek, binding tenantID and sessionID
// as AAD so a sealed payload cannot be replayed onto a different event row
// without detection.
func Seal(dek DEK, plaintext []byte, tenantID, sessionID string) ([]byte, error) {
	aad := []byte(tenantID + "|" + sessionID)
	return sealWithKey(dek.Key, plaintext, aad)
}

// Open reverses Seal.
func Open(dek DEK, sealed []byte, tenantID, sessionID string) ([]byte, error) {
	aad := []byte(tenantID + "|" + sessionID)
	return openWithKey(dek.Key, sealed, aad)
}

// Digest returns a digest over PLAINTEXT — independent of any key, so it
// survives crypto-shredding (FR-081): the audit chain can still verify a
// shredded event's digest even after its content is unrecoverable.
func Digest(plaintext []byte) []byte {
	sum := sha256.Sum256(plaintext)
	return sum[:]
}
