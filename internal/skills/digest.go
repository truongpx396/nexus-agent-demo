package skills

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"slices"
)

// BundleDigest hashes a bundle's behavior-bearing content in a fixed,
// sequential order — never map iteration, matching every other digest
// function in this codebase (internal/tools.descriptorDigest,
// internal/harness.Digest). Content-addressed (README task 7.3): the same
// bundle contents always produce the same digest, which is what
// sign.VerifySignature signs over and BuildSkillSet folds into
// harness.Config.SkillSetDigest.
func BundleDigest(b SkillBundle) []byte {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "skill_id=%s\n", b.SkillID)
	_, _ = fmt.Fprintf(h, "description=%s\n", b.Description)
	_, _ = fmt.Fprintf(h, "trigger_hint=%s\n", b.TriggerHint)

	for _, id := range slices.Sorted(slices.Values(b.DeclaredToolIDs)) {
		_, _ = fmt.Fprintf(h, "declared_tool_id=%s\n", id)
	}

	for _, f := range slices.SortedFunc(slices.Values(b.Files), func(a, b BundleFile) int { return cmp.Compare(a.Path, b.Path) }) {
		contentDigest := sha256.Sum256(f.Content)
		_, _ = fmt.Fprintf(h, "file_path=%s\n", f.Path)
		_, _ = fmt.Fprintf(h, "file_content_sha256=%x\n", contentDigest)
	}

	_, _ = fmt.Fprintf(h, "script_present=%v\n", b.HasScript())
	if b.HasScript() {
		scriptDigest := sha256.Sum256(b.ScriptContent)
		_, _ = fmt.Fprintf(h, "script_sha256=%x\n", scriptDigest)
	}

	return h.Sum(nil)
}
