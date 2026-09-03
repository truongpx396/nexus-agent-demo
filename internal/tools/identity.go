package tools

import (
	"fmt"
	"regexp"
	"sort"
)

// ToolRef is the qualified tool identity (README task 3.1, pattern 13):
// {namespace}/{name}@{version}. Every field is required — there is no
// unversioned or unnamespaced identity anywhere in this system.
type ToolRef struct {
	Namespace string
	Name      string
	Version   string
}

func (r ToolRef) String() string {
	return r.Namespace + "/" + r.Name + "@" + r.Version
}

// Namespace may itself be multiple `/`-separated segments (Phase 11, README
// task 11.1): a remote MCP server's tools are qualified as
// "mcp/{server}/{tool}@{version}" — Namespace is the whole "mcp/{server}"
// prefix, one declared-and-owned namespace per admitted server, so two
// servers never collide under the single shared Registry. Every namespace
// that predates Phase 11 ("platform", "skill", ...) is a single segment and
// parses identically to before — this widens the grammar, it does not change
// it for anything already using it.
var refPattern = regexp.MustCompile(`^([a-z0-9_-]+(?:/[a-z0-9_-]+)*)/([a-z0-9_-]+)@(v?[0-9][a-zA-Z0-9_.-]*)$`)

// ParseToolRef parses the wire/string form. Failing loud on a malformed ref
// is deliberate — the pipeline's step 1 (resolve) must never guess at an
// identity it can't fully parse.
func ParseToolRef(s string) (ToolRef, error) {
	m := refPattern.FindStringSubmatch(s)
	if m == nil {
		return ToolRef{}, fmt.Errorf("tools: %q is not a valid {namespace}/{name}@{version} tool ref", s)
	}
	return ToolRef{Namespace: m[1], Name: m[2], Version: m[3]}, nil
}

// Registry is the resident catalog: every tool this process can dispatch,
// keyed by its qualified ref, admission-refused on a namespace collision.
// "One owner per namespace" (README task 3.1) means every namespace has a
// single declared owner, and only that owner may register a tool into it —
// a second owner attempting the same namespace is refused at Register time,
// never resolved by "whoever registered first wins."
type Registry struct {
	namespaceOwner map[string]string    // namespace -> owner
	tools          map[string]toolEntry // ToolRef.String() -> entry
	byNamespace    map[string][]string  // namespace -> []ToolRef.String(), insertion order
}

type toolEntry struct {
	tool   Tool
	status AdmissionStatus
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		namespaceOwner: map[string]string{},
		tools:          map[string]toolEntry{},
		byNamespace:    map[string][]string{},
	}
}

// DeclareNamespace binds owner as the single owner of namespace. Declaring
// the same namespace with a different owner is refused — the collision
// admission refuses, not registration order arbitrating a winner.
func (r *Registry) DeclareNamespace(namespace, owner string) error {
	if existing, ok := r.namespaceOwner[namespace]; ok && existing != owner {
		return fmt.Errorf("tools: namespace %q is already owned by %q, refusing to also bind %q", namespace, existing, owner)
	}
	r.namespaceOwner[namespace] = owner
	return nil
}

// Register admits tool into the catalog. The tool's namespace must already
// be declared (DeclareNamespace) — an undeclared namespace has no owner to
// check a collision against, so it is refused rather than silently
// auto-owned by the first registrant.
func (r *Registry) Register(t Tool) error {
	ref := t.ID()
	if _, ok := r.namespaceOwner[ref.Namespace]; !ok {
		return fmt.Errorf("tools: namespace %q has no declared owner; call DeclareNamespace before Register", ref.Namespace)
	}
	key := ref.String()
	if _, exists := r.tools[key]; exists {
		return fmt.Errorf("tools: %q is already registered", key)
	}
	r.tools[key] = toolEntry{tool: t, status: AdmissionPending}
	r.byNamespace[ref.Namespace] = append(r.byNamespace[ref.Namespace], key)
	return nil
}

// Lookup resolves a qualified ref to its registered Tool.
func (r *Registry) Lookup(ref ToolRef) (Tool, bool) {
	e, ok := r.tools[ref.String()]
	if !ok {
		return nil, false
	}
	return e.tool, true
}

// SetAdmissionStatus records the cached verdict admit.go's scanner produced
// for one tool — the pipeline's admission gate (step 3) reads this rather
// than re-scanning on every call.
func (r *Registry) SetAdmissionStatus(ref ToolRef, status AdmissionStatus) error {
	key := ref.String()
	e, ok := r.tools[key]
	if !ok {
		return fmt.Errorf("tools: %q is not registered", key)
	}
	e.status = status
	r.tools[key] = e
	return nil
}

// AdmissionStatus reports the cached verdict for a registered tool.
func (r *Registry) AdmissionStatus(ref ToolRef) (AdmissionStatus, bool) {
	e, ok := r.tools[ref.String()]
	if !ok {
		return "", false
	}
	return e.status, true
}

// All returns every registered tool, in deterministic (namespace, then
// registration) order — manifest.go's Build depends on that determinism to
// produce a stable digest.
func (r *Registry) All() []Tool {
	var namespaces []string
	for ns := range r.byNamespace {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	var out []Tool
	for _, ns := range namespaces {
		for _, key := range r.byNamespace[ns] {
			out = append(out, r.tools[key].tool)
		}
	}
	return out
}
