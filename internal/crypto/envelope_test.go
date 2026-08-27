package crypto

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	dek, err := GenerateDEK("key-1")
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	plaintext := []byte(`{"body":"hello"}`)

	sealed, err := Seal(dek, plaintext, "tenant-a", "session-1")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, plaintext) {
		t.Fatal("sealed output contains the plaintext verbatim")
	}

	got, err := Open(dek, sealed, "tenant-a", "session-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Open = %q, want %q", got, plaintext)
	}
}

func TestOpenFailsUnderWrongAAD(t *testing.T) {
	dek, err := GenerateDEK("key-1")
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	sealed, err := Seal(dek, []byte("secret"), "tenant-a", "session-1")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// A sealed payload bound to (tenant-a, session-1) must not open under a
	// different session — this is what stops a ciphertext being replayed
	// onto a different event row.
	if _, err := Open(dek, sealed, "tenant-a", "session-2"); err == nil {
		t.Fatal("Open succeeded under a mismatched AAD binding")
	}
}

func TestWrapUnwrapDEKRoundTrip(t *testing.T) {
	kek, err := GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	dek, err := GenerateDEK("key-42")
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}

	wrapped, err := WrapDEK(kek, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if bytes.Contains(wrapped, dek.Key) {
		t.Fatal("wrapped DEK contains the raw key bytes verbatim")
	}

	got, err := UnwrapDEK(kek, "key-42", wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(got.Key, dek.Key) {
		t.Fatal("UnwrapDEK did not recover the original key bytes")
	}
}

func TestUnwrapDEKFailsUnderWrongKeyID(t *testing.T) {
	kek, err := GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	dek, err := GenerateDEK("key-42")
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	wrapped, err := WrapDEK(kek, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}

	if _, err := UnwrapDEK(kek, "some-other-key-id", wrapped); err == nil {
		t.Fatal("UnwrapDEK succeeded with a keyID the wrapped DEK was not bound to")
	}
}

func TestDigestIsDeterministicAndKeyIndependent(t *testing.T) {
	plaintext := []byte("the event payload, in the clear")

	d1 := Digest(plaintext)
	d2 := Digest(plaintext)
	if !bytes.Equal(d1, d2) {
		t.Fatal("Digest is not deterministic")
	}

	other := Digest([]byte("a different payload"))
	if bytes.Equal(d1, other) {
		t.Fatal("Digest did not distinguish different plaintexts")
	}
}
