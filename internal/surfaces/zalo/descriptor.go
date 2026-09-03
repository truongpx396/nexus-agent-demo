package zalo

import "github.com/truongpx396/nexus-agent-demo/internal/surfaces/capability"

// Descriptor is internal/surfaces/telegram.Descriptor's own counterpart —
// a chat bot has no rich approval-context UI and no server-push streaming
// channel of its own.
var Descriptor = capability.Descriptor{
	SurfaceID:                "zalo",
	PrincipalKind:            capability.PrincipalUser,
	CanRenderApprovalContext: false,
	SupportsStepUp:           false,
	SupportsStructuredInput:  false,
	SupportsStreaming:        false,
}
