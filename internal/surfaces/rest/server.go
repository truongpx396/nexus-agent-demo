// Package rest is the REST surface's shared kit: the HTTP server, bearer-token
// authentication, CORS, and the seam each feature mounts its own routes through.
// It holds no endpoint. The endpoints belong to the features they expose, each in
// its own rest package beside the feature (internal/runs/rest, internal/oversight/
// rest, internal/runctl/rest, internal/contentaccess/rest), the way each feature's
// storage sits in its postgres package; cmd/nexusd decides which of them are
// mounted.
//
// The router is Gin. It is confined to this kit and to the feature rest packages
// that mount on it: Server.Handler returns a plain http.Handler, so cmd/nexusd,
// the webhooks and the probes outside the REST API stay on net/http.
//
// The routes are described once, in api/openapi.yaml, and oapi-codegen turns the
// operations a feature owns into that feature's models and its ServerInterface
// (api.gen.go beside its handlers). A feature's Routes registers the generated
// interface on Router.Authenticated.
//
// A handler is a thin translator only: it decodes the request, calls its
// feature's use case, and encodes what comes back. It holds no agent control
// flow of its own (constitution Principle I), and this package never imports the
// kernel (tests/contract/boundaries_test.go).
//
// AuthN (README task 13.1): every route a Module mounts through Router requires a
// verified bearer token — the calling principal comes only from Verifier.Verify's
// claims, never a client-supplied header. See authn.go.
//
// A handler passes c.Request.Context(), never c itself, to anything that waits:
// a *gin.Context is reused once the handler returns, and its own Done is nil
// unless the engine opts into falling back to the request's.
package rest

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Module is one feature's REST endpoints. Routes registers them on r, which wraps
// each in authentication, so a feature cannot mount an unauthenticated route by
// forgetting to.
type Module interface {
	Routes(r Router)
}

// Router is what a Module registers its routes on. Every way to add a route
// (GET, POST, and the router Authenticated returns) authenticates it.
type Router struct {
	engine *gin.Engine
	srv    *Server
}

// GET registers h for GET path (Gin's path syntax, "/v1/approvals/:id"), behind
// the bearer-token check and the request-body bound.
func (r Router) GET(path string, h gin.HandlerFunc) { r.handle(http.MethodGet, path, h) }

// POST registers h for POST path, behind the bearer-token check and the
// request-body bound.
func (r Router) POST(path string, h gin.HandlerFunc) { r.handle(http.MethodPost, path, h) }

// Authenticated returns the router a generated server registers on
// (RegisterHandlersWithOptions in a feature's api.gen.go, built from
// api/openapi.yaml). It is a Gin route group that carries the same bearer-token
// check and body bound GET and POST attach, so a route registered on it cannot
// skip either, and a path no module mounts still gets the engine's own 404.
func (r Router) Authenticated() gin.IRouter {
	return r.engine.Group("", r.srv.authenticate(), boundBody)
}

// handle attaches authentication and the body bound to the one route rather than
// to the engine (engine.Use), so a path no module mounts still gets the router's
// own 404 without needing a token first.
func (r Router) handle(method, path string, h gin.HandlerFunc) {
	r.engine.Handle(method, path, r.srv.authenticate(), boundBody, h)
}

// maxRequestBody bounds every request body a module reads. The server's
// ReadTimeout bounds how long a client may take to send one, not how much: a
// JSON decoder buffers a whole value, so without this a single authenticated
// request could make the process allocate until it is killed.
const maxRequestBody = 4 << 20

func boundBody(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBody)
}

// DecodeJSON decodes the request body into v. On failure it has already
// answered, 413 for a body over the limit and 400 for anything else, so the
// handler just returns.
func DecodeJSON(c *gin.Context, v any) bool {
	if err := c.ShouldBindJSON(v); err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			Error(c, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			Error(c, "invalid JSON body", http.StatusBadRequest)
		}
		return false
	}
	return true
}

// ParamError is the ErrorHandler of a generated server's options
// (GinServerOptions.ErrorHandler): a path parameter that does not parse answers
// err's one-line message with status, in plain text like every other error here.
func ParamError(c *gin.Context, err error, status int) {
	Error(c, err.Error(), status)
}

// Error answers with msg as a plain-text body and status, and ends the handler
// chain. It is http.Error for a Gin handler: the same Content-Type, nosniff
// header and trailing newline every client of this API already sees.
func Error(c *gin.Context, msg string, status int) {
	c.Abort()
	http.Error(c.Writer, msg, status)
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
// individually wrapped in authenticate (README task 13.1) — an unregistered
// path still gets the router's own 404 without needing a token first.
func (s *Server) Handler() http.Handler {
	return withCORS(s.engine())
}

// Route is one method and path a Server answers. The path is in OpenAPI's
// syntax ("/v1/approvals/{id}"), so it compares directly with api/openapi.yaml.
type Route struct {
	Method, Path string
}

var pathParam = regexp.MustCompile(`:([A-Za-z_][A-Za-z0-9_]*)`)

// Routes lists every route the modules mount. It exists so a test can hold
// api/openapi.yaml to what is actually served, in both directions.
func (s *Server) Routes() []Route {
	infos := s.engine().Routes()
	routes := make([]Route, len(infos))
	for i, ri := range infos {
		routes[i] = Route{Method: ri.Method, Path: pathParam.ReplaceAllString(ri.Path, "{$1}")}
	}
	return routes
}

// engine builds the Gin engine with every module's routes mounted.
func (s *Server) engine() *gin.Engine {
	// Release mode: debug mode prints every route to stdout, outside the
	// structured log.
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	// Answer what net/http's mux answered: 405 with an Allow header for a known
	// path on the wrong method, and 404, never a redirect, for a path that
	// differs only by a trailing slash (a redirect would re-send a request body
	// to a route the caller did not ask for).
	engine.HandleMethodNotAllowed = true
	engine.RedirectTrailingSlash = false
	engine.RedirectFixedPath = false

	r := Router{engine: engine, srv: s}
	for _, m := range s.modules {
		m.Routes(r)
	}
	return engine
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
// package's own authenticate would otherwise produce. It wraps the engine
// rather than being a Gin middleware so a preflight, which matches no route, is
// answered before routing.
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

// Principal reads the calling identity authenticate already verified and stored
// on c. It writes the response itself on failure, so call sites just check ok —
// in practice this only fails if authenticate somehow didn't run, since every
// route a Module mounts is wrapped by it.
func Principal(c *gin.Context) (tenantID, userID uuid.UUID, ok bool) {
	tenantID, userID, ok = principalFrom(c)
	if !ok {
		Error(c, "no verified principal on request context", http.StatusUnauthorized)
		return uuid.Nil, uuid.Nil, false
	}
	return tenantID, userID, true
}
