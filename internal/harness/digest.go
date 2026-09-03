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

	// MCPCatalogDigest folds in one tenant's admitted-MCP-server tool
	// listing (README task 11.1) the same way SkillSetDigest already folds
	// in a tenant's admitted skill set — a per-tenant addition to the
	// resolvable tool universe is behavior-bearing config like any other
	// (task 3.2, pattern 14), so a session's digest must move if a remote
	// server's tools do. internal/surfaces/mcp.Port.Digest computes it.
	MCPCatalogDigest []byte
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
	_, _ = fmt.Fprintf(h, "mcp_catalog_digest=%x\n", c.MCPCatalogDigest)
	return h.Sum(nil)
}
