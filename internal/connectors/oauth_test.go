package connectors

import (
	"testing"

	"github.com/google/uuid"
)

// TestRandomState is the CSRF state token every BeginAuth call mints —
// must be non-empty, hex-encoded, and not repeat across calls.
func TestRandomState(t *testing.T) {
	a, err := randomState()
	if err != nil {
		t.Fatalf("randomState: %v", err)
	}
	b, err := randomState()
	if err != nil {
		t.Fatalf("randomState: %v", err)
	}
	if a == "" || b == "" {
		t.Fatal("randomState returned an empty string")
	}
	if a == b {
		t.Error("randomState returned the same value twice in a row")
	}
	if len(a) != 40 { // 20 random bytes, hex-encoded
		t.Errorf("randomState length = %d, want 40 (20 bytes hex-encoded)", len(a))
	}
}

// TestAADLabel proves the AAD binds a sealed token to BOTH the user and the
// provider — a token sealed under one user/provider pair must never open
// under a different one (the actual security property this label exists
// for; internal/crypto.Open verifies the AAD, this just proves the label
// itself varies with both inputs).
func TestAADLabel(t *testing.T) {
	userA, userB := uuid.New(), uuid.New()
	base := aadLabel(userA, "github")
	if base != aadLabel(userA, "github") {
		t.Fatal("aadLabel is not deterministic for identical inputs")
	}
	if base == aadLabel(userB, "github") {
		t.Error("aadLabel did not change with a different userID")
	}
	if base == aadLabel(userA, "gitlab") {
		t.Error("aadLabel did not change with a different provider name")
	}
}
