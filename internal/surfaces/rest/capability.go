package rest

import (
	"fmt"
	"net/http"

	"github.com/google/uuid"

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
// first-class, typed step: read fresh off THIS request's headers, every
// time — never cached from a prior request or inherited from whoever
// opened a long-lived connection. Functionally identical to principal()
// above (same headers, same errors); this is the typed entry point new
// Phase 7 call sites use, kept alongside principal() rather than replacing
// it everywhere to avoid an unrelated churn across every existing handler.
func (s *Server) resolvePrincipal(r *http.Request) (capability.Principal, error) {
	tenantID, err := uuid.Parse(r.Header.Get("X-Nexus-Tenant-ID"))
	if err != nil {
		return capability.Principal{}, fmt.Errorf("missing or invalid X-Nexus-Tenant-ID header")
	}
	userID, err := uuid.Parse(r.Header.Get("X-Nexus-User-ID"))
	if err != nil {
		return capability.Principal{}, fmt.Errorf("missing or invalid X-Nexus-User-ID header")
	}
	return capability.Principal{Kind: capability.PrincipalUser, TenantID: tenantID, UserID: userID}, nil
}
