package cron

import "github.com/truongpx396/nexus-agent-demo/internal/surfaces/capability"

// Descriptor declares this surface's static capability (README task 7.12,
// 11.8's conformance suite): cron is the pre-declared PrincipalScheduler's
// first real consumer (internal/surfaces/capability.go) — there is no
// human on the other end of a scheduled run to render approval context to
// or stream events at; an approval it raises waits for a human on whatever
// surface actually reviews approvals (REST/web).
var Descriptor = capability.Descriptor{
	SurfaceID:                "cron",
	PrincipalKind:            capability.PrincipalScheduler,
	CanRenderApprovalContext: false,
	SupportsStepUp:           false,
	SupportsStructuredInput:  false,
	SupportsStreaming:        false,
}
