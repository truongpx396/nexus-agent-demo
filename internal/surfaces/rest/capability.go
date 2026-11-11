package rest

import (
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/capability"
)

// Descriptor is this surface's static capability declaration (README task
// 7.12) — REST can render full structured approval context, accepts
// structured (JSON) input, and streams a run's events over SSE
// (internal/runs/rest's handleEvents). Never per-request; a conformance test
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

// ResolvePrincipal is task 7.13's per-turn principal resolution made a
// first-class, typed step: read fresh off THIS request's verified context,
// every time — never cached from a prior request or inherited from whoever
// opened a long-lived connection. Functionally identical to Principal
// (server.go) — both read what authenticate already verified — as a typed
// capability.Principal for the handler that starts a turn.
func ResolvePrincipal(c *gin.Context) (capability.Principal, error) {
	tenantID, userID, ok := principalFrom(c)
	if !ok {
		return capability.Principal{}, errors.New("no verified principal on request context")
	}
	return capability.Principal{Kind: capability.PrincipalUser, TenantID: tenantID, UserID: userID}, nil
}
