// Package web declares the React web app's capability descriptor (README
// Phase 11, task 11.7) for task 11.8's conformance suite — the app itself
// is a standalone npm project (web/) with no Go code of its own, since it
// talks only to the existing, unmodified REST API (this package's own doc
// comment on that, and 11.7's own claim: "No backend surface change —
// proves the API was surface-agnostic all along"). This Go package exists
// purely so "all eight surfaces declare a capability the same way" is
// literally true, including the one with no Go server code behind it.
package web

import "github.com/truongpx396/nexus-agent-demo/internal/surfaces/capability"

// Descriptor mirrors REST's own (internal/surfaces/rest/capability.go) —
// the web app is a full-fidelity client of the exact same API REST itself
// serves, so it can render structured approval context, accept structured
// input, and stream events (via its own fetch-based SSE reader, since a
// browser's native EventSource cannot carry the auth headers this API
// requires — web/src/lib/sse.ts).
var Descriptor = capability.Descriptor{
	SurfaceID:                "web",
	PrincipalKind:            capability.PrincipalUser,
	CanRenderApprovalContext: true,
	SupportsStepUp:           false,
	SupportsStructuredInput:  true,
	SupportsStreaming:        true,
}
