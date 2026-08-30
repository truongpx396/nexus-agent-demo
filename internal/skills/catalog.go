package skills

import "fmt"

// scriptFileName is the reserved bundle-relative name a script is loaded
// under — never returned by ReadFile (task 7.9: tier 3 is reference content
// only; a script is admitted as its own tool, task 7.5, and invoked as its
// own tool_use, never fetched as a file read).
const scriptFileName = "script"

// Catalog is the process-wide, trust-filtered set of skill bundles this
// nexusd instance actually holds — every bundle in it already passed
// ScanBundle and VerifySignature (cmd/nexusd's loader enforces that before
// constructing one); Catalog itself does no further trust filtering, only
// per-tenant admission (checked by the caller via internal/config, not
// here — a Catalog is shared process-wide, admission is per-tenant).
type Catalog struct {
	bundles map[string]SkillBundle
}

// NewCatalog indexes bundles by SkillID. A duplicate SkillID across bundles
// is a loader-time configuration error, not something Catalog silently
// resolves — the last one wins here only because cmd/nexusd's loader is
// expected to refuse the whole load on a collision before ever reaching
// this constructor.
func NewCatalog(bundles []SkillBundle) *Catalog {
	c := &Catalog{bundles: make(map[string]SkillBundle, len(bundles))}
	for _, b := range bundles {
		c.bundles[b.SkillID] = b
	}
	return c
}

// Resolve returns the full bundle for skillID — used by
// internal/tools/builtin.ActivateSkill to produce tier 2 (the body).
func (c *Catalog) Resolve(skillID string) (SkillBundle, bool) {
	b, ok := c.bundles[skillID]
	return b, ok
}

// ReadFile returns one reference file's content — tier 3, lazy, never the
// script (task 7.9).
func (c *Catalog) ReadFile(skillID, path string) ([]byte, error) {
	if path == scriptFileName {
		return nil, fmt.Errorf("skills: %q is the bundle's script, not a reference file", path)
	}
	b, ok := c.bundles[skillID]
	if !ok {
		return nil, fmt.Errorf("skills: unknown skill %q", skillID)
	}
	for _, f := range b.Files {
		if f.Path == path {
			return f.Content, nil
		}
	}
	return nil, fmt.Errorf("skills: skill %q has no file %q", skillID, path)
}
