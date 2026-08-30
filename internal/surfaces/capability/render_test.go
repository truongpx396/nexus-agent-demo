package capability

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderApprovalContext_FullCapabilityRendersStructuredInput(t *testing.T) {
	d := Descriptor{SurfaceID: "rest", CanRenderApprovalContext: true, SupportsStructuredInput: true}
	ctx := ContextPackage{ToolID: "platform/web_fetch@v1", EffectClass: "external", Input: json.RawMessage(`{"url":"https://example.com"}`)}

	out := RenderApprovalContext(d, ctx)
	if !strings.Contains(out, "platform/web_fetch@v1") || !strings.Contains(out, "https://example.com") {
		t.Errorf("RenderApprovalContext with full capability = %q, want the tool id and raw input rendered", out)
	}
}

func TestRenderApprovalContext_MinimalCapabilityFallsBackToASummary(t *testing.T) {
	d := Descriptor{SurfaceID: "minimal-webhook", CanRenderApprovalContext: false, SupportsStructuredInput: false}
	ctx := ContextPackage{ToolID: "platform/web_fetch@v1", EffectClass: "external", Input: json.RawMessage(`{"url":"https://example.com"}`)}

	out := RenderApprovalContext(d, ctx)
	if strings.Contains(out, "https://example.com") {
		t.Errorf("RenderApprovalContext with minimal capability = %q, want no raw input leaked into the fallback", out)
	}
	if !strings.Contains(out, "platform/web_fetch@v1") {
		t.Errorf("RenderApprovalContext fallback = %q, want at least the tool id named", out)
	}
}

func TestRenderApprovalContext_NeverABareIdentifierAlone(t *testing.T) {
	// README §5's own demo language: "never a bare UUID." Neither rendering
	// path should ever be JUST an opaque identifier with no other context.
	d := Descriptor{}
	ctx := ContextPackage{ToolID: "platform/shell@v1", EffectClass: "mutating"}
	out := RenderApprovalContext(d, ctx)
	if out == ctx.ToolID {
		t.Errorf("RenderApprovalContext = %q, want more than a bare identifier", out)
	}
}
