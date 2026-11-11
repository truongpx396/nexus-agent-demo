package rest

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// PrincipalVerifier is the seam between this surface and internal/authn —
// declared locally (structurally identical to authn.Verifier) so this
// package never imports internal/authn directly, the same
// never-import-the-concrete-implementation-package idiom the feature
// handlers' own ports (SkillSetPort, MCPPort, ...) use. Swapping the
// dev Ed25519 issuer for a real OIDC provider (Casdoor, Keycloak, ...) later
// is purely a cmd/nexusd wiring change: any type with this one method
// satisfies this interface without rest ever knowing what backs it.
type PrincipalVerifier interface {
	Verify(tokenString string) (tenantID, userID uuid.UUID, err error)
}

// principalKey is where authenticate stores the verified principal on the Gin
// context. It is unexported, and this file is the only writer, so a handler
// cannot be handed a principal by anything but authenticate.
const principalKey = "nexus.principal"

// resolvedPrincipal is what authenticate stores on the Gin context —
// the ONLY place a tenantID/userID pair enters request handling from here on
// (README task 13.1 deleted the header-reading path Principal/ResolvePrincipal
// used to implement).
type resolvedPrincipal struct {
	tenantID, userID uuid.UUID
}

// authenticate verifies the Authorization: Bearer <token> header on every
// request and stores the resulting principal on the Gin context — 401
// on a missing header, a token Verifier rejects, or a nil Verifier (auth is
// mandatory, never nil-valid, unlike this package's other optional Ports).
//
// Router attaches it to each route it registers, not to the engine, so an
// UNREGISTERED route still gets the router's own native 404 without needing a
// valid token first (attaching it engine-wide would turn every 404 into a 401,
// which is also what breaks the "404 means unmounted" conformance-test technique
// capability_test.go already relies on).
func (s *Server) authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.Verifier == nil {
			Error(c, "server misconfigured: no principal verifier wired", http.StatusInternalServerError)
			return
		}
		token, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok || token == "" {
			Error(c, "missing or invalid Authorization header (want: Bearer <token>)", http.StatusUnauthorized)
			return
		}
		tenantID, userID, err := s.Verifier.Verify(token)
		if err != nil {
			Error(c, "invalid or expired token", http.StatusUnauthorized)
			return
		}
		c.Set(principalKey, resolvedPrincipal{tenantID: tenantID, userID: userID})
		c.Next()
	}
}

// principalFrom reads back what authenticate stored — the single
// implementation behind both Principal (server.go) and ResolvePrincipal
// (capability.go).
func principalFrom(c *gin.Context) (tenantID, userID uuid.UUID, ok bool) {
	v, _ := c.Get(principalKey)
	p, ok := v.(resolvedPrincipal)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	return p.tenantID, p.userID, true
}
