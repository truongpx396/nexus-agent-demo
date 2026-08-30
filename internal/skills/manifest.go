package skills

import (
	"crypto/sha256"
	"fmt"
	"sort"
)

// ResidentMetadata is tier 1 (README task 7.6): what the model sees about an
// admitted skill without activating it — never the body, never the
// reference files.
type ResidentMetadata struct {
	SkillID         string
	Description     string
	TriggerHint     string
	DeclaredToolIDs []string
}

// SkillSet is the tenant's admitted, resident skill catalog for one session
// — the skills analog of internal/tools.Manifest. Digest folds into
// internal/harness.Config.SkillSetDigest at run start, exactly like
// CatalogManifestDigest already does for tools.
type SkillSet struct {
	Entries []ResidentMetadata
	Digest  []byte
}

// BuildSkillSet keeps only bundles whose SkillID is in admittedIDs — the
// caller (cmd/nexusd) is responsible for having already dropped any bundle
// that failed ScanBundle or VerifySignature, so this function only applies
// the tenant-admission filter, not the trust filter. Digest is computed over
// entries sorted by SkillID, in the same fixed-order-sha256 style every
// other digest in this codebase uses.
func BuildSkillSet(bundles []SkillBundle, admittedIDs []string) SkillSet {
	admitted := make(map[string]bool, len(admittedIDs))
	for _, id := range admittedIDs {
		admitted[id] = true
	}

	var set SkillSet
	for _, b := range bundles {
		if !admitted[b.SkillID] {
			continue
		}
		set.Entries = append(set.Entries, ResidentMetadata{
			SkillID:         b.SkillID,
			Description:     b.Description,
			TriggerHint:     b.TriggerHint,
			DeclaredToolIDs: append([]string(nil), b.DeclaredToolIDs...),
		})
	}
	sort.Slice(set.Entries, func(i, j int) bool { return set.Entries[i].SkillID < set.Entries[j].SkillID })

	h := sha256.New()
	for _, e := range set.Entries {
		_, _ = fmt.Fprintf(h, "skill_id=%s\n", e.SkillID)
		_, _ = fmt.Fprintf(h, "description=%s\n", e.Description)
		_, _ = fmt.Fprintf(h, "trigger_hint=%s\n", e.TriggerHint)
		ids := append([]string(nil), e.DeclaredToolIDs...)
		sort.Strings(ids)
		for _, id := range ids {
			_, _ = fmt.Fprintf(h, "declared_tool_id=%s\n", id)
		}
	}
	set.Digest = h.Sum(nil)
	return set
}
