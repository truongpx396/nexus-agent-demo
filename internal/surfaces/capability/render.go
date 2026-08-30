package capability

import (
	"encoding/json"
	"fmt"
)

// ContextPackage mirrors internal/oversight.ContextPackage's shape —
// duplicated rather than imported, the same "structural duplication
// instead of a cross-boundary import" idiom internal/surfaces/rest's own
// SealFunc/RunEvent already use for kernel.SealFunc/kernel's event shape.
// This package must stay free of an internal/oversight dependency: a
// future low-capability surface (Phase 11's cron, say) needs this type
// without pulling in oversight's kernel-adjacent import chain.
type ContextPackage struct {
	ToolID      string          `json:"tool_id"`
	EffectClass string          `json:"effect_class,omitempty"`
	Input       json.RawMessage `json:"input"`
}

// RenderApprovalContext is "approval routing filters on capability" made
// concrete (task 7.12): a surface that can render structured approval
// context gets the full decision-ready breakdown (tool id, effect class,
// and the actual input — never a bare UUID, README §5's own demo
// language); one that can't gets a minimal one-line fallback instead. No
// surface in this phase actually needs the fallback branch yet (both REST
// and CLI can render structured context) — it exists so a future minimal
// surface (a webhook ack, say) has a defined, tested behavior on day one
// rather than an afterthought once one exists.
func RenderApprovalContext(d Descriptor, ctx ContextPackage) string {
	if !d.CanRenderApprovalContext || !d.SupportsStructuredInput {
		return fmt.Sprintf("approval pending for %s (%s) — view full context on a surface that supports it", ctx.ToolID, ctx.EffectClass)
	}
	return fmt.Sprintf("tool_id=%s effect_class=%s input=%s", ctx.ToolID, ctx.EffectClass, string(ctx.Input))
}
