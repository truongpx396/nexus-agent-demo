package email

import "github.com/truongpx396/nexus-agent-demo/internal/surfaces/capability"

// Descriptor: an email thread has no rich approval-context UI and no
// server-push streaming channel of its own — the same shape
// internal/surfaces/telegram/zalo's own Descriptors already declare.
var Descriptor = capability.Descriptor{
	SurfaceID:                "email",
	PrincipalKind:            capability.PrincipalUser,
	CanRenderApprovalContext: false,
	SupportsStepUp:           false,
	SupportsStructuredInput:  false,
	SupportsStreaming:        false,
}
