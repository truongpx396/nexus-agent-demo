package permissions

import "strings"

// matchGlob is the same small matcher internal/hooks uses for its Matcher
// field, duplicated rather than imported (this package stays a leaf with no
// dependency on internal/hooks — chain.go only ever receives a hook's
// already-computed LayerOutcome, never the hooks package itself). "*" or ""
// matches everything; "ns/*" matches every tool in namespace ns; anything
// else must equal the full tool ref or the bare namespace exactly.
func matchGlob(pattern, toolID, namespace string) bool {
	switch {
	case pattern == "" || pattern == "*":
		return true
	case pattern == toolID:
		return true
	case strings.HasSuffix(pattern, "/*"):
		return strings.TrimSuffix(pattern, "/*") == namespace
	default:
		return pattern == namespace
	}
}
