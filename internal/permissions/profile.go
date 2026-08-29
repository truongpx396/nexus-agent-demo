package permissions

import "fmt"

// ToolProfile is Gate 1 (README task 3.8, pattern 19): a named, versioned
// tenant config binding a set of tools. Membership resolves only DENY or
// DEFER — never ALLOW; being a member says "the remaining layers may still
// speak," not "this call is permitted."
type ToolProfile struct {
	Name    string
	Version int
	tools   map[string]bool
}

// NewToolProfile builds a profile from a set of exact ToolRef strings.
func NewToolProfile(name string, version int, toolRefs ...string) ToolProfile {
	m := make(map[string]bool, len(toolRefs))
	for _, t := range toolRefs {
		m[t] = true
	}
	return ToolProfile{Name: name, Version: version, tools: m}
}

// Has reports whether toolID is a declared member of this profile.
func (p ToolProfile) Has(toolID string) bool { return p.tools[toolID] }

// ProfileSet is every profile bound to one session — a tool is a Gate 1
// member if any bound profile includes it.
type ProfileSet struct {
	Profiles []ToolProfile
}

// Resolve is layer 4's evaluation.
func (s ProfileSet) Resolve(toolID string) LayerOutcome {
	for _, p := range s.Profiles {
		if p.Has(toolID) {
			return LayerOutcome{Decision: Defer, Reason: fmt.Sprintf("member of tool profile %s@v%d", p.Name, p.Version)}
		}
	}
	return LayerOutcome{Decision: Deny, Reason: "not a member of any tool profile bound to this session"}
}
