package runs_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
)

func newDEK(t *testing.T) crypto.DEK {
	t.Helper()
	dek, err := crypto.GenerateDEK("test-key")
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	return dek
}

func TestSealFor_RoundTripsForItsOwnSessionOnly(t *testing.T) {
	dek := newDEK(t)
	tenantID, sessionID := uuid.New(), uuid.New()
	plaintext := []byte(`{"body":"hello"}`)

	sealed, digest, keyID, err := runs.SealFor(dek, tenantID, sessionID)(plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if keyID != dek.KeyID {
		t.Errorf("keyID = %q, want the DEK's %q", keyID, dek.KeyID)
	}
	if string(digest) != string(crypto.Digest(plaintext)) {
		t.Errorf("digest is not over the plaintext")
	}

	got, err := crypto.Open(dek, sealed, tenantID.String(), sessionID.String())
	if err != nil || string(got) != string(plaintext) {
		t.Fatalf("Open with the same ids = %q, %v; want the plaintext back", got, err)
	}
	if _, err := crypto.Open(dek, sealed, tenantID.String(), uuid.NewString()); err == nil {
		t.Error("an event sealed for one session opened as another's: the session id is not bound into the ciphertext")
	}
	if _, err := crypto.Open(dek, sealed, uuid.NewString(), sessionID.String()); err == nil {
		t.Error("an event sealed for one tenant opened as another's: the tenant id is not bound into the ciphertext")
	}
}
