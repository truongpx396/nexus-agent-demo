// Package skills is signed, content-addressed skill bundles with
// three-tier disclosure (README task 7.3, pattern #47): tier 1 is resident
// metadata folded into harness_digest at run start (manifest.go); tier 2 is
// a skill's body, delivered as an ordinary tool_result when
// internal/tools/builtin.ActivateSkill runs (no new kernel ABI — see that
// file's own doc comment); tier 3 is a bundled file fetched lazily via
// internal/tools/builtin.ReadSkillFile. A bundle's script (if any) is a real
// tool admitted through the ordinary tools.Registry gates, or the whole
// bundle is refused (task 7.5) — this package never executes anything
// itself.
package skills

// BundleFile is one reference/template file inside a bundle — never the
// bundle's script, which is registered as a tool (task 7.5), not read as
// tier-3 content.
type BundleFile struct {
	Path    string
	Content []byte
}

// SkillBundle is one skill: its identity, its tier-1/tier-2 text, the tool
// ids it wants (checked, never trusted — README task 7.4/7.8), its
// reference files, and the signature over all of it.
type SkillBundle struct {
	SkillID         string
	Description     string
	TriggerHint     string
	DeclaredToolIDs []string
	Files           []BundleFile
	Signature       []byte

	// ScriptContent is the bundle's own tool, if it has one (task 7.5) — nil
	// for a bundle with no script. The loader returns this separately from
	// Files because a script is registered into a tools.Registry, never
	// read as reference content (catalog.go's ReadFile explicitly refuses
	// the reserved name "script").
	ScriptContent []byte
}

// HasScript reports whether this bundle carries its own tool.
func (b SkillBundle) HasScript() bool { return len(b.ScriptContent) > 0 }
