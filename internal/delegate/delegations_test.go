package delegate

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
)

// TestNullableJSON and TestNullIfEmpty are README task 13.8 (closing
// production-readiness finding F9): internal/delegate shipped with zero
// _test.go files despite owning delegation scope descent and taint ascend.
// These two are the pure helpers every DB-facing insert in this package
// goes through — small, but exactly the kind of thing worth pinning.
func TestNullableJSON(t *testing.T) {
	if got := nullableJSON(nil); got != nil {
		t.Errorf("nullableJSON(nil) = %v, want nil", got)
	}
	if got := nullableJSON(json.RawMessage{}); got != nil {
		t.Errorf("nullableJSON(empty) = %v, want nil", got)
	}
	in := json.RawMessage(`{"a":1}`)
	if got := nullableJSON(in); !bytes.Equal(got, in) {
		t.Errorf("nullableJSON(%s) = %s, want it returned verbatim", in, got)
	}
}

func TestNullIfEmpty(t *testing.T) {
	if got := nullIfEmpty(""); got != nil {
		t.Errorf("nullIfEmpty(\"\") = %v, want nil", got)
	}
	got := nullIfEmpty("agent-1")
	if got == nil || *got != "agent-1" {
		t.Errorf("nullIfEmpty(\"agent-1\") = %v, want a pointer to \"agent-1\"", got)
	}
}

// TestSealFuncFor_RoundTrips is README task 13.8: sealFuncFor is what every
// child-session event delegate.go appends goes through — a fast, in-memory
// round trip (no DB, no KeyStore) over the same crypto.Seal/Open pair the
// real DEK-backed path uses, proving the closure it returns actually seals
// what it's given and reports a stable digest/key id.
func TestSealFuncFor_RoundTrips(t *testing.T) {
	dek, err := crypto.GenerateDEK("test-key-1")
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	tenantID, sessionID := uuid.New(), uuid.New()
	seal := sealFuncFor(dek, tenantID, sessionID)

	plaintext := []byte(`{"tool_name":"delegate"}`)
	sealed, digest, keyID, err := seal(plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if keyID != dek.KeyID {
		t.Errorf("keyID = %q, want %q", keyID, dek.KeyID)
	}
	if !bytes.Equal(digest, crypto.Digest(plaintext)) {
		t.Error("digest does not match crypto.Digest(plaintext)")
	}

	opened, err := crypto.Open(dek, sealed, tenantID.String(), sessionID.String())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Errorf("round trip = %q, want %q", opened, plaintext)
	}

	// Sealing under the WRONG tenant/session (the AAD) must fail to open —
	// this is what actually scopes an event to its own session.
	if _, err := crypto.Open(dek, sealed, uuid.New().String(), sessionID.String()); err == nil {
		t.Error("Open succeeded with the wrong tenantID AAD — sealed data is not scoped correctly")
	}
}
