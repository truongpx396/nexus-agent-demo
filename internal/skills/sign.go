package skills

import "crypto/ed25519"

// VerifySignature checks b.Signature against BundleDigest(b) — signing is a
// precondition for admission, not a scan finding: a bundle with a missing or
// invalid signature is refused regardless of what ScanBundle says about its
// text. Bundles are signed offline before being placed on disk (this demo's
// stand-in for a real publish/promote pipeline — README's own Principle IX
// language, "propose -> human/eval gate -> version -> promote," names the
// process this checks the output of, not the process itself).
func VerifySignature(b SkillBundle, pub ed25519.PublicKey) bool {
	if len(pub) != ed25519.PublicKeySize || len(b.Signature) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, BundleDigest(b), b.Signature)
}
