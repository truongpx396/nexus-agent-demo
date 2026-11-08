// Package rest is the REST surface's shared kit: the HTTP server, bearer-token
// authentication, CORS, and the seam each feature mounts its own routes through.
// It holds no endpoint. The endpoints belong to the features they expose, each in
// its own rest package beside the feature (internal/runs/rest, internal/oversight/
// rest, internal/runctl/rest, internal/contentaccess/rest), the way each feature's
// storage sits in its postgres package; cmd/nexusd decides which of them are
// mounted.
//
// A handler is a thin translator only: it decodes the request, calls its
// feature's use case, and encodes what comes back. It holds no agent control
// flow of its own (constitution Principle I), and this package never imports the
// kernel (tests/contract/boundaries_test.go).
//
// AuthN (README task 13.1): every route a Module mounts through Router.Handle
// requires a verified bearer token — the calling principal comes only from
// Verifier.Verify's claims, never a client-supplied header. See authn.go.
package rest

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

// Module is one feature's REST endpoints. Routes registers them on r, which wraps
// each in authentication, so a feature cannot mount an unauthenticated route by
// forgetting to.
type Module interface {
	Routes(r Router)
}

// Router is what a Module registers its routes on. The only way to add a route is
// Handle, which authenticates it.
type Router struct {
	mux *http.ServeMux
	srv *Server
}

// Handle registers h for pattern (a net/http ServeMux pattern such as
// "GET /v1/approvals/{id}"), behind the bearer-token check.
func (r Router) Handle(pattern string, h http.HandlerFunc) {
	r.mux.Handle(pattern, r.srv.authed(h))
}

// Server serves the modules it was given over HTTP.
type Server struct {
	// Verifier resolves the bearer token on every request to the principal
	// submitting it (README task 13.1). It is mandatory: a nil Verifier fails
	// every request closed (401/500), it never falls back to trusting a
	// client-supplied header.
	Verifier PrincipalVerifier

	modules []Module
}

// NewServer returns a Server that authenticates with verifier and serves the
// routes of modules. A feature whose module is not passed has no routes, so its
// paths answer 404, which is how a capability that is not wired shows as absent.
func NewServer(verifier PrincipalVerifier, modules ...Module) *Server {
	return &Server{Verifier: verifier, modules: modules}
}

// Handler returns the http.Handler cmd/nexusd mounts. Every route is
// individually wrapped in authMiddleware (README task 13.1) — an unregistered
// path still gets the mux's own native 404 without needing a token first.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	r := Router{mux: mux, srv: s}
	for _, m := range s.modules {
		m.Routes(r)
	}
	return withCORS(mux)
}

// withCORS lets a browser-based client (web/, typically served from a
// different origin/port than nexusd during local dev -- Vite's dev server
// on :5173 talking to nexusd on :8085) actually call this API: this
// package's own auth is a bearer token the CLIENT chooses to attach, never
// a cookie the browser attaches automatically, so there is no CSRF exposure
// reflecting the request's own Origin creates (unlike cookie auth, where
// that would matter) -- the same reasoning that makes permissive CORS
// standard practice for a bearer-token API. Without this, every request
// from a real browser fails outright: the browser's own preflight OPTIONS
// request gets nexusd's default 405 (no route registers OPTIONS), which
// surfaces to fetch() as an opaque "Failed to fetch," not the 401/403 this
// package's own authMiddleware would otherwise produce.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			reqHeaders := r.Header.Get("Access-Control-Request-Headers")
			if reqHeaders == "" {
				reqHeaders = "authorization, content-type"
			}
			w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Principal reads the calling identity authMiddleware already verified and
// stored on the request context. It writes the response itself on failure,
// mirroring the shape http.Error already uses, so call sites just check ok —
// in practice this only fails if authMiddleware somehow didn't run, since every
// route a Module mounts is wrapped by it.
func Principal(w http.ResponseWriter, r *http.Request) (tenantID, userID uuid.UUID, ok bool) {
	tenantID, userID, ok = principalFromContext(r.Context())
	if !ok {
		http.Error(w, "no verified principal on request context", http.StatusUnauthorized)
		return uuid.Nil, uuid.Nil, false
	}
	return tenantID, userID, true
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
