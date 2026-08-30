package skills

import (
	"crypto/sha256"
	"fmt"
	"sort"
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

	toolIDs := append([]string(nil), b.DeclaredToolIDs...)
	sort.Strings(toolIDs)
	for _, id := range toolIDs {
		_, _ = fmt.Fprintf(h, "declared_tool_id=%s\n", id)
	}

	files := append([]BundleFile(nil), b.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
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
