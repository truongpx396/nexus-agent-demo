package web

import "testing"

// TestDescriptor_ClaimsFullFidelity is this surface's conformance
// statement (README task 11.8): the web app is documented as a
// full-fidelity REST client (web/README.md, web/src/lib/sse.ts's
// fetch-based SSE reader), so unlike the chat/scheduler surfaces its
// Descriptor claims real support for approval context, structured input,
// and streaming rather than degrading to a fallback.
func TestDescriptor_ClaimsFullFidelity(t *testing.T) {
	if !Descriptor.CanRenderApprovalContext {
		t.Error("Descriptor.CanRenderApprovalContext = false, want true")
	}
	if !Descriptor.SupportsStructuredInput {
		t.Error("Descriptor.SupportsStructuredInput = false, want true")
	}
	if !Descriptor.SupportsStreaming {
		t.Error("Descriptor.SupportsStreaming = false, want true")
	}
	if Descriptor.SurfaceID != "web" {
		t.Errorf("Descriptor.SurfaceID = %q, want %q", Descriptor.SurfaceID, "web")
	}
}
