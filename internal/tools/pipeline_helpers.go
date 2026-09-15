package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
)

func joinReason(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "; " + addition
}

func toPermissionsTaint(t Taint) permissions.Taint {
	return permissions.Taint{
		ReturnsUntrusted: t.ReturnsUntrusted,
		ReadsPrivateData: t.ReadsPrivateData,
		MutatesExternal:  t.MutatesExternal,
	}
}

// toLayerOutcome translates a wire-level decision string (from a Tool's own
// PermissionResult or a hooks.Outcome) into a permissions.LayerOutcome.
// Allow is refused here, at the translation boundary, with a descriptive
// error — internal/permissions.Chain.Resolve also guards against it, but
// failing at the point of translation names which precomputed layer
// produced the violation.
func toLayerOutcome(decision, reason string) (permissions.LayerOutcome, error) {
	switch permissions.Decision(decision) {
	case permissions.Deny, permissions.Ask, permissions.Defer:
		return permissions.LayerOutcome{Decision: permissions.Decision(decision), Reason: reason}, nil
	case permissions.Allow:
		return permissions.LayerOutcome{}, fmt.Errorf("a precomputed layer resolved Allow (%q), which is never valid", reason)
	default:
		return permissions.LayerOutcome{}, fmt.Errorf("unrecognized decision %q", decision)
	}
}

// safeCall recovers a panicking Tool.Call into a typed error — "a tool must
// never crash the kernel loop" (pipeline.go's step 13 doc comment above).
func safeCall(ctx context.Context, tool Tool, input json.RawMessage, rc RunContext) (result Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool panicked: %v", r)
		}
	}()
	return tool.Call(ctx, input, rc)
}
