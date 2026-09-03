package telegram

import "github.com/truongpx396/nexus-agent-demo/internal/surfaces/capability"

// Descriptor declares this surface's static capability (README task 7.12,
// 11.8's conformance suite extended to it): a chat bot has no rich
// approval-context UI and no server-push streaming channel of its own — an
// approval requested mid-run reaches the user as a plain-text notification
// via the outbox (capability.RenderApprovalContext's one-line fallback,
// the exact scenario that branch exists for), and the user checks the
// run's outcome by asking again or via another surface, not by watching a
// live stream inside Telegram.
var Descriptor = capability.Descriptor{
	SurfaceID:                "telegram",
	PrincipalKind:            capability.PrincipalUser,
	CanRenderApprovalContext: false,
	SupportsStepUp:           false,
	SupportsStructuredInput:  false,
	SupportsStreaming:        false,
}
