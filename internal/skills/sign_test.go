package skills

import (
	"crypto/ed25519"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	b := SkillBundle{SkillID: "s", Description: "d", Files: []BundleFile{{Path: "a.md", Content: []byte("v1")}}}
	b.Signature = ed25519.Sign(priv, BundleDigest(b))

	if !VerifySignature(b, pub) {
		t.Error("VerifySignature = false for a correctly signed bundle, want true")
	}

	tampered := b
	tampered.Description = "a different description entirely"
	if VerifySignature(tampered, pub) {
		t.Error("VerifySignature = true for a bundle whose content changed after signing, want false")
	}

	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if VerifySignature(b, otherPub) {
		t.Error("VerifySignature = true against the wrong public key, want false")
	}

	unsigned := SkillBundle{SkillID: "s", Description: "d"}
	if VerifySignature(unsigned, pub) {
		t.Error("VerifySignature = true for an unsigned bundle, want false")
	}
}
