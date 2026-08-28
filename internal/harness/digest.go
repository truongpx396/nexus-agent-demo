// Package harness computes the harness_digest pinned onto every session at
// run start: the identity of all behavior-determining configuration in
// force (docs/constitution.md, FR-129). It is also the cache-prefix
// identity the byte-stable prompt prefix (Phase 2) is measured against, and
// what a fork (Phase 6) compares to detect configuration divergence rather
// than silently presenting a different configuration's result as a
// reproduction.
package harness

import (
	"crypto/sha256"
	"fmt"
)

// Config is every input the digest is computed over. Fields are added, not
// repurposed, as later phases introduce more behavior-bearing config (the
// resolved tool catalog manifest, the skill set, the approval policy
// version) — each is a seam this type already has room for.
type Config struct {
	SystemPromptVersion   string
	CatalogManifestDigest []byte
	SkillSetDigest        []byte
	SafetyPolicyVersion   string
	ApprovalPolicyVersion string
	PromptMode            string
}

// Digest returns a stable digest over c. Encoding order is fixed and
// explicit (never map iteration, which Go does not guarantee is stable)
// specifically so the same Config always produces the same digest.
func Digest(c Config) []byte {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "system_prompt_version=%s\n", c.SystemPromptVersion)
	_, _ = fmt.Fprintf(h, "catalog_manifest_digest=%x\n", c.CatalogManifestDigest)
	_, _ = fmt.Fprintf(h, "skill_set_digest=%x\n", c.SkillSetDigest)
	_, _ = fmt.Fprintf(h, "safety_policy_version=%s\n", c.SafetyPolicyVersion)
	_, _ = fmt.Fprintf(h, "approval_policy_version=%s\n", c.ApprovalPolicyVersion)
	_, _ = fmt.Fprintf(h, "prompt_mode=%s\n", c.PromptMode)
	return h.Sum(nil)
}
