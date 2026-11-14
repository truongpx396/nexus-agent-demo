package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/authn"
)

var requiredOutsideDev = []string{
	"NEXUS_DATABASE_URL", "NEXUS_REDIS_ADDR", "NEXUS_SIGNERD_SOCKET", "NEXUS_KEK_PATH",
	"NEXUS_AUTHN_PUBLIC_KEY_PATH", "NEXUS_ADMIN_DATABASE_URL", "NEXUS_METRICS_TOKEN",
}

// Outside --dev the server does not start without the admin DSN, the metrics token
// and the public key it verifies tokens with, and it is not asked for the signing key.
func TestLoadServerConfig_OutsideDevRequiresWhatItNeedsAndNotTheSigningKey(t *testing.T) {
	for _, k := range append([]string{"NEXUS_AUTHN_SIGNING_KEY_PATH"}, requiredOutsideDev...) {
		t.Setenv(k, "")
	}
	_, err := loadServerConfig(false)
	if err == nil {
		t.Fatal("loadServerConfig(false) with nothing set = nil error")
	}
	for _, want := range requiredOutsideDev {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not list %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "NEXUS_AUTHN_SIGNING_KEY_PATH") {
		t.Errorf("error %q asks the server for the signing key, which it must never hold", err)
	}

	values := map[string]string{
		"NEXUS_DATABASE_URL": "postgres://app", "NEXUS_REDIS_ADDR": "redis:6379", "NEXUS_SIGNERD_SOCKET": "/run/signerd.sock",
		"NEXUS_KEK_PATH": "/keys/kek", "NEXUS_AUTHN_PUBLIC_KEY_PATH": "/keys/authn.pub",
		"NEXUS_ADMIN_DATABASE_URL": "postgres://admin", "NEXUS_METRICS_TOKEN": "tok",
	}
	for k, v := range values {
		t.Setenv(k, v)
	}
	cfg, err := loadServerConfig(false)
	if err != nil {
		t.Fatalf("loadServerConfig(false) with everything set and no signing key: %v", err)
	}
	if cfg.AdminDatabaseURL != "postgres://admin" || cfg.MetricsToken != "tok" || cfg.AuthnPublicKeyPath != "/keys/authn.pub" || cfg.AuthnSigningKeyPath != "" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

// Either a Redis URL (password, TLS) or the plain address satisfies the requirement.
func TestLoadServerConfig_ARedisURLInPlaceOfTheAddressIsEnough(t *testing.T) {
	for k, v := range map[string]string{ //nolint:gosec // a URL fixture, not a credential
		"NEXUS_DATABASE_URL": "postgres://app", "NEXUS_REDIS_ADDR": "", "NEXUS_REDIS_URL": "rediss://u:p@redis:6380/0",
		"NEXUS_SIGNERD_SOCKET": "/run/signerd.sock", "NEXUS_KEK_PATH": "/keys/kek", "NEXUS_AUTHN_PUBLIC_KEY_PATH": "/keys/authn.pub",
		"NEXUS_ADMIN_DATABASE_URL": "postgres://admin", "NEXUS_METRICS_TOKEN": "tok",
	} {
		t.Setenv(k, v)
	}
	cfg, err := loadServerConfig(false)
	if err != nil {
		t.Fatalf("loadServerConfig with NEXUS_REDIS_URL and no NEXUS_REDIS_ADDR: %v", err)
	}
	if cfg.RedisURL != "rediss://u:p@redis:6380/0" { //nolint:gosec // a URL fixture, not a credential
		t.Fatalf("cfg.RedisURL = %q", cfg.RedisURL)
	}

	t.Setenv("NEXUS_REDIS_URL", "")
	if _, err := loadServerConfig(false); err == nil || !strings.Contains(err.Error(), "NEXUS_REDIS_URL or NEXUS_REDIS_ADDR") {
		t.Fatalf("with neither: err = %v, want it to name both", err)
	}
}

func TestLoadServerConfig_DevKeepsTheDefaultsAndTheNewSettingsAreOptional(t *testing.T) {
	for _, k := range []string{"NEXUS_ADMIN_DATABASE_URL", "NEXUS_MIGRATE_DATABASE_URL", "NEXUS_METRICS_TOKEN", "NEXUS_AUTHN_PUBLIC_KEY_PATH", "NEXUS_AUTHN_SIGNING_KEY_PATH"} {
		t.Setenv(k, "")
	}
	cfg, err := loadServerConfig(true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminDatabaseURL != defaultMigrateDSN || cfg.MetricsToken != "" {
		t.Fatalf("dev cfg admin=%q token=%q, want the local default and no token", cfg.AdminDatabaseURL, cfg.MetricsToken)
	}
	if cfg.AuthnPublicKeyPath != "" || cfg.AuthnSigningKeyPath != defaultAuthnSigningKeyPath {
		t.Fatalf("dev cfg public=%q signing=%q, want no public key file and the dev signing key", cfg.AuthnPublicKeyPath, cfg.AuthnSigningKeyPath)
	}
}

// signingKeyFile writes a fresh Ed25519 signing key the way the dev setup does.
func signingKeyFile(t *testing.T) (path string, issuer *authn.DevIssuer) {
	t.Helper()
	priv, err := authn.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "authn_signing.key")
	if err := os.WriteFile(path, priv, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, authn.NewDevIssuer(priv)
}

// With a public key file configured, that is what verifies tokens, and the signing
// key is never read.
func TestLoadVerifyingKey_UsesThePublicKeyFileAndNeverReadsTheSigningKey(t *testing.T) {
	signingPath, issuer := signingKeyFile(t)
	pubPath := filepath.Join(t.TempDir(), "authn.pub")
	t.Setenv("NEXUS_AUTHN_SIGNING_KEY_PATH", signingPath)
	if err := runToken(t.Context(), []string{"--public-key-out", pubPath}); err != nil {
		t.Fatalf("token --public-key-out: %v", err)
	}

	// The signing key file is gone from the server's host: only the public half is configured.
	if err := os.Remove(signingPath); err != nil {
		t.Fatal(err)
	}
	key, err := loadVerifyingKey(serverConfig{AuthnPublicKeyPath: pubPath, AuthnSigningKeyPath: signingPath})
	if err != nil {
		t.Fatalf("loadVerifyingKey: %v", err)
	}

	tenant, user := uuid.New(), uuid.New()
	tok, err := issuer.Issue(tenant, user, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	gotTenant, gotUser, err := authn.NewDevVerifier(key).Verify(tok)
	if err != nil || gotTenant != tenant || gotUser != user {
		t.Fatalf("Verify = %s, %s, %v", gotTenant, gotUser, err)
	}
}

// Under --dev, with no public key file, the verifying key is derived from the dev
// signing key, so `make up && make run` still needs no setup.
func TestLoadVerifyingKey_DevDerivesItFromTheSigningKey(t *testing.T) {
	signingPath, issuer := signingKeyFile(t)
	key, err := loadVerifyingKey(serverConfig{AuthnSigningKeyPath: signingPath})
	if err != nil {
		t.Fatalf("loadVerifyingKey: %v", err)
	}
	tok, err := issuer.Issue(uuid.New(), uuid.New(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := authn.NewDevVerifier(key).Verify(tok); err != nil {
		t.Fatalf("a token minted by the dev signing key does not verify against the derived key: %v", err)
	}
}

func TestLoadVerifyingKey_RefusesAMissingOrWrongSizedPublicKeyFile(t *testing.T) {
	if _, err := loadVerifyingKey(serverConfig{AuthnPublicKeyPath: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Error("a missing public key file was accepted")
	}
	// The private key file passed by mistake must not be read as a public key.
	signingPath, _ := signingKeyFile(t)
	if _, err := loadVerifyingKey(serverConfig{AuthnPublicKeyPath: signingPath}); err == nil {
		t.Error("the 64-byte private key file was accepted as a public key")
	}
}

func TestRunToken_PublicKeyOutWritesA0644FileAndStillMintsATokenWithATenant(t *testing.T) {
	signingPath, _ := signingKeyFile(t)
	t.Setenv("NEXUS_AUTHN_SIGNING_KEY_PATH", signingPath)
	pubPath := filepath.Join(t.TempDir(), "authn.pub")

	if err := runToken(t.Context(), []string{"--tenant", "acme", "--public-key-out", pubPath}); err != nil {
		t.Fatalf("runToken: %v", err)
	}
	info, err := os.Stat(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 || info.Size() != 32 {
		t.Fatalf("public key file mode %v size %d, want 0644 and 32 bytes", info.Mode().Perm(), info.Size())
	}

	if err := runToken(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "--tenant is required") {
		t.Fatalf("runToken with neither flag = %v, want the --tenant error", err)
	}
}
