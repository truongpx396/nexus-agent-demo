package rest

import (
	"fmt"
	"net/http"

	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/capability"
)

// Descriptor is this surface's static capability declaration (README task
// 7.12) — REST can render full structured approval context, accepts
// structured (JSON) input, and streams a run's events over SSE
// (handleEvents). Never per-request; a conformance test
// (capability_test.go) checks these claims against the surface's actual
// behavior.
var Descriptor = capability.Descriptor{
	SurfaceID:                "rest",
	PrincipalKind:            capability.PrincipalUser,
	CanRenderApprovalContext: true,
	SupportsStepUp:           false,
	SupportsStructuredInput:  true,
	SupportsStreaming:        true,
}

// resolvePrincipal is task 7.13's per-turn principal resolution made a
// first-class, typed step: read fresh off THIS request's verified context,
// every time — never cached from a prior request or inherited from whoever
// opened a long-lived connection. Functionally identical to principal()
// (server.go) — both read what authMiddleware already verified — kept as a
// separate typed entry point to avoid an unrelated rename churn across every
// existing handler.
func (s *Server) resolvePrincipal(r *http.Request) (capability.Principal, error) {
	tenantID, userID, ok := principalFromContext(r.Context())
	if !ok {
		return capability.Principal{}, fmt.Errorf("no verified principal on request context")
	}
	return capability.Principal{Kind: capability.PrincipalUser, TenantID: tenantID, UserID: userID}, nil
}
