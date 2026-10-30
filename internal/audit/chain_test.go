package audit

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

// TestHashInput_DeterministicAndSensitiveToEveryField is README task 13.8
// (closing production-readiness finding F9): HashInput is the actual
// hash-chain math (pattern #31) — a fast, DB-free unit test on it catches a
// chain-breaking regression far earlier than the integration verify tests.
func TestHashInput_DeterministicAndSensitiveToEveryField(t *testing.T) {
	prev := bytes.Repeat([]byte{0xAB}, 32)
	tenantID, sessionID, eventID := uuid.New(), uuid.New(), uuid.New()
	digest := bytes.Repeat([]byte{0xCD}, 32)

	base := HashInput(prev, tenantID, sessionID, 1, eventID, "tool_use", digest)
	again := HashInput(prev, tenantID, sessionID, 1, eventID, "tool_use", digest)
	if !bytes.Equal(base, again) {
		t.Fatal("HashInput is not deterministic for identical inputs")
	}
	if len(base) != 32 {
		t.Fatalf("HashInput returned %d bytes, want 32 (sha256)", len(base))
	}

	variants := map[string][]byte{
		"different prevHash":      HashInput(bytes.Repeat([]byte{0xEF}, 32), tenantID, sessionID, 1, eventID, "tool_use", digest),
		"different tenantID":      HashInput(prev, uuid.New(), sessionID, 1, eventID, "tool_use", digest),
		"different sessionID":     HashInput(prev, tenantID, uuid.New(), 1, eventID, "tool_use", digest),
		"different seq":           HashInput(prev, tenantID, sessionID, 2, eventID, "tool_use", digest),
		"different eventID":       HashInput(prev, tenantID, sessionID, 1, uuid.New(), "tool_use", digest),
		"different eventType":     HashInput(prev, tenantID, sessionID, 1, eventID, "tool_result", digest),
		"different payloadDigest": HashInput(prev, tenantID, sessionID, 1, eventID, "tool_use", bytes.Repeat([]byte{0x11}, 32)),
	}
	for name, variant := range variants {
		if bytes.Equal(base, variant) {
			t.Errorf("%s produced the SAME hash as the base input — HashInput is not sensitive to this field", name)
		}
	}
}

// TestHashInput_NilPrevHashUsesGenesis proves a session's first receipt
// (PrevHash == nil) hashes identically to explicitly passing the 32-byte
// zero genesis value — the fixed-width encoding HashInput's own doc comment
// promises, never a variable-length/nil-vs-empty ambiguity.
func TestHashInput_NilPrevHashUsesGenesis(t *testing.T) {
	tenantID, sessionID, eventID := uuid.New(), uuid.New(), uuid.New()
	digest := bytes.Repeat([]byte{0x01}, 32)

	withNil := HashInput(nil, tenantID, sessionID, 1, eventID, "tool_use", digest)
	withGenesis := HashInput(GenesisPrevHash, tenantID, sessionID, 1, eventID, "tool_use", digest)
	if !bytes.Equal(withNil, withGenesis) {
		t.Fatal("HashInput(nil, ...) does not match HashInput(GenesisPrevHash, ...)")
	}
}

// TestHashInput_EventTypeLengthPrefixPreventsCollision proves the
// length-prefix on eventType (HashInput's own doc comment: "no
// delimiter-based string concatenation that two different inputs could
// collide into") actually does its job — "ab"+"c" and "a"+"bc" must hash
// differently even though naive concatenation would make them identical
// byte sequences.
func TestHashInput_EventTypeLengthPrefixPreventsCollision(t *testing.T) {
	tenantID, sessionID, eventID := uuid.New(), uuid.New(), uuid.New()
	prev := bytes.Repeat([]byte{0x00}, 32)

	a := HashInput(prev, tenantID, sessionID, 1, eventID, "ab", []byte("c"))
	b := HashInput(prev, tenantID, sessionID, 1, eventID, "a", []byte("bc"))
	if bytes.Equal(a, b) {
		t.Fatal("HashInput collided across an eventType/payloadDigest boundary shift — the length prefix isn't doing its job")
	}
}

func TestReport_OK(t *testing.T) {
	if !(Report{}).OK() {
		t.Error("an empty Report should be OK")
	}
	if (Report{Breaks: []Break{{Kind: "hash_mismatch"}}}).OK() {
		t.Error("a Report with a Break should not be OK")
	}
	if (Report{Gaps: []Gap{{MissingSeq: 5}}}).OK() {
		t.Error("a Report with a Gap should not be OK")
	}
}
