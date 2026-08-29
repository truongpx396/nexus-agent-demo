package tools

import (
	"crypto/sha256"
	"fmt"
)

// ManifestEntry pins one tool's identity and descriptor shape.
// DescriptorDigest is what pipeline step 2 (digest re-verify) compares a
// live Tool's Descriptor() against at call time, so a catalog that changed
// shape mid-session is caught rather than silently used.
type ManifestEntry struct {
	ID               ToolRef
	DescriptorDigest []byte
}

// Manifest is the catalog manifest (README task 3.2, pattern 14): the
// *resolvable* universe for one session, pinned once at session start and
// folded into harness_digest (internal/harness.Config.CatalogManifestDigest)
// — resolving a tool outside this set, or one whose live descriptor no
// longer matches what was pinned, is what step 1/2 of the pipeline refuse.
type Manifest struct {
	Entries []ManifestEntry
	Digest  []byte
}

// descriptorDigest hashes a Descriptor's behavior-bearing fields — never its
// EffectClass alone; the whole point is to catch a shape change end to end.
func descriptorDigest(d Descriptor) []byte {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "id=%s\n", d.ID)
	_, _ = fmt.Fprintf(h, "description=%s\n", d.Description)
	_, _ = fmt.Fprintf(h, "input_schema=%s\n", string(d.InputSchema))
	_, _ = fmt.Fprintf(h, "effect_class=%s\n", d.EffectClass)
	return h.Sum(nil)
}

// BuildManifest snapshots reg's current, deterministically-ordered contents
// (Registry.All's doc comment) into a Manifest. Only AdmissionClean tools
// are included — a manifest is "the resolvable universe," and a
// flagged/rejected/still-pending tool is not resolvable by definition.
func BuildManifest(reg *Registry) Manifest {
	var m Manifest
	h := sha256.New()
	for _, t := range reg.All() {
		status, _ := reg.AdmissionStatus(t.ID())
		if status != AdmissionClean {
			continue
		}
		d := t.Descriptor()
		digest := descriptorDigest(d)
		m.Entries = append(m.Entries, ManifestEntry{ID: t.ID(), DescriptorDigest: digest})
		_, _ = fmt.Fprintf(h, "%s:%x\n", t.ID(), digest)
	}
	m.Digest = h.Sum(nil)
	return m
}

// Resolve looks up ref within the pinned manifest — pipeline step 1. It
// deliberately does not consult the live Registry: resolving against what
// was pinned at session start (not against whatever the catalog looks like
// right now) is what "pins the resolvable universe" means.
func (m Manifest) Resolve(ref ToolRef) (ManifestEntry, bool) {
	for _, e := range m.Entries {
		if e.ID == ref {
			return e, true
		}
	}
	return ManifestEntry{}, false
}
