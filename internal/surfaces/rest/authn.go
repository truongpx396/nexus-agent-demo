package rest

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// PrincipalVerifier is the seam between this surface and internal/authn —
// declared locally (structurally identical to authn.Verifier) so this
// package never imports internal/authn directly, the same
// never-import-the-concrete-implementation-package idiom OversightPort/
// RunCtlPort/SkillSetPort/MCPPort already use one file over. Swapping the
// dev Ed25519 issuer for a real OIDC provider (Casdoor, Keycloak, ...) later
// is purely a cmd/nexusd wiring change: any type with this one method
// satisfies this interface without rest ever knowing what backs it.
type PrincipalVerifier interface {
	Verify(tokenString string) (tenantID, userID uuid.UUID, err error)
}

type principalContextKey struct{}

// resolvedPrincipal is what authMiddleware stores in the request context —
// the ONLY place a tenantID/userID pair enters this package's request
// handling from here on (README task 13.1 deletes the header-reading path
// principal()/resolvePrincipal() used to implement).
type resolvedPrincipal struct {
	tenantID, userID uuid.UUID
}

// authed wraps one handler with authMiddleware — the per-route registration
// helper Handler() uses for every route it mounts, so an UNREGISTERED route
// still gets the mux's own native 404 without needing a valid token first
// (wrapping the whole mux instead would turn every 404 into a 401, which is
// also what breaks the "404 means unmounted" conformance-test technique
// capability_test.go already relies on).
func (s *Server) authed(h http.HandlerFunc) http.Handler {
	return s.authMiddleware(h)
}

// authMiddleware verifies the Authorization: Bearer <token> header on every
// request and stores the resulting principal in the request context — 401
// on a missing header, a token Verifier rejects, or a nil Verifier (auth is
// mandatory, never nil-valid, unlike this package's other optional Ports).
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Verifier == nil {
			http.Error(w, "server misconfigured: no principal verifier wired", http.StatusInternalServerError)
			return
		}
		auth := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok || token == "" {
			http.Error(w, "missing or invalid Authorization header (want: Bearer <token>)", http.StatusUnauthorized)
			return
		}
		tenantID, userID, err := s.Verifier.Verify(token)
		if err != nil {
			http.Error(w, "invalid or expired token", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), principalContextKey{}, resolvedPrincipal{tenantID: tenantID, userID: userID})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// principalFromContext reads back what authMiddleware stored — the single
// implementation behind both principal() (server.go) and resolvePrincipal()
// (capability.go), kept as two call-site names only to avoid an unrelated
// rename churn across every existing handler.
func principalFromContext(ctx context.Context) (tenantID, userID uuid.UUID, ok bool) {
	p, ok := ctx.Value(principalContextKey{}).(resolvedPrincipal)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	return p.tenantID, p.userID, true
}
