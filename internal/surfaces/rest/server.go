// Package rest is the REST surface (README task 2.10): POST /v1/runs, GET
// /v1/runs/{id}, GET /v1/runs/{id}/events. A thin translator only — it
// creates a session, hands it to a RunStarter (starter.go), and forwards
// what comes back; it holds no agent control flow of its own (constitution
// Principle I) and never imports the kernel package directly
// (tests/contract/boundaries_test.go).
//
// AuthN (README task 13.1): every route Handler() mounts requires a verified
// bearer token — the calling principal comes only from Verifier.Verify's
// claims, never a client-supplied header. See authn.go.
package rest

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/controlplane"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces"
)

// Server holds everything one REST process needs to admit and serve runs
// over the RunStarter it wraps.
type Server struct {
	Starter  RunStarter
	Store    *store.Store
	KeyStore *crypto.KeyStore

	// Verifier resolves the bearer token on every request to the principal
	// submitting it (README task 13.1) — mandatory, unlike every other Port
	// on this struct: a nil Verifier fails every request closed (401/500),
	// it never falls back to trusting a client-supplied header.
	Verifier PrincipalVerifier

	// ControlPlane admits a new run (README task 13.15) — the session row
	// plus an optional session-scoped budget, through
	// internal/controlplane.Port, the versioned control-plane boundary
	// README §2 has always described. Mandatory, like Verifier: a nil
	// ControlPlane fails handleCreateRun closed (500) rather than panic.
	// internal/controlplane imports nothing this package doesn't already
	// (it's a deliberately dependency-light contract package), so — like
	// Grants — this is a direct import, no translation seam needed.
	ControlPlane controlplane.Port

	// CatalogManifestDigest is folded into every new session's
	// harness_digest (internal/harness.Config.CatalogManifestDigest) — the
	// resolvable tool universe is behavior-bearing config like any other
	// (README task 3.2, pattern 14), so a session's digest must move if the
	// resident catalog does.
	CatalogManifestDigest []byte

	// Oversight, if set, backs the approval endpoints (README task 5.6-5.8)
	// — nil leaves them unmounted, which every pre-Phase-5 caller and test
	// gets.
	Oversight OversightPort

	// Grants, if set, backs the content-access-grant endpoints (README
	// task 5.11). internal/obs has no reason to import kernel, so — unlike
	// Oversight — this package imports it directly; no translation seam is
	// needed (tests/contract/boundaries_test.go's transitive kernel-import
	// check on internal/surfaces is what actually decides which
	// dependencies need one, not this package's own convenience).
	Grants *obs.Grants

	// RunCtl, if set, backs the cancel/steer/tighten-autonomy/fork endpoints
	// (README task 6.9-6.11) — nil leaves them unmounted, which every
	// pre-Phase-6 caller and test gets.
	RunCtl RunCtlPort

	// Skills, if set, resolves one tenant's admitted-skill-set digest at
	// session-creation time (README task 7.6) — nil leaves
	// harness.Config.SkillSetDigest at its pre-Phase-7 zero value, which
	// every earlier caller and test still gets. Unlike CatalogManifestDigest
	// (fixed once at process startup — the resident tool catalog is
	// process-wide), a skill set is per-tenant, so this has to be resolved
	// per request rather than baked into a Server field.
	Skills SkillSetPort

	// Outbox/OutboxSender, if both set, back durable at-least-once delivery
	// (README task 7.14) of one event class — EventApprovalRequested — that
	// genuinely needs it: a human must actually see this to act, unlike the
	// broker's best-effort SSE fan-out to whichever clients happen to be
	// connected right now. Nil leaves this unmounted, which every
	// pre-Phase-7 caller and test still gets; a real Telegram/email sender
	// is Phase 11's.
	Outbox       *surfaces.Outbox
	OutboxSender surfaces.Sender

	// MCP, if set, resolves one tenant's admitted MCP tool set at
	// session-creation time (README task 11.1) — nil leaves it unmounted,
	// which every pre-Phase-11 caller and test still gets. Unlike
	// CatalogManifestDigest (process-wide, fixed at startup), a tenant's
	// MCP catalog is per-tenant and per-user (an oauth_connector-authed
	// server authenticates as the calling user), so it's resolved fresh
	// per run the same way Skills already is.
	MCP MCPPort

	// Exporter, if set, emits one content-free span per completed run
	// (README task 13.12) — session.id/tenant.id/terminal_reason only, the
	// same allowlist-filtered shape internal/obs's stdout Exporter and
	// OTLPExporter both already enforce; nil leaves this unmounted, which
	// every pre-Phase-13 caller and test still gets.
	Exporter SpanEmitter

	// Lock, if set, serializes handleSteerRun's own resume-vs-steer
	// decision and the ResumeConversation turn it may kick off, keyed on
	// the session's own session_key (falling back to its session_id when
	// unset — see handleSteerRun's own doc comment) — the SAME
	// *queue.SessionLock instance cmd/nexusd wires into every webhook
	// surface's own Lock field, so a REST-driven steer and a
	// webhook-driven resume of the SAME conversation contend for the SAME
	// Redis key (production-readiness review: "the REST/webhook
	// direct-call path bypasses [SessionLock] entirely"). nil (every
	// pre-this-fix caller and test) reproduces the prior unlocked
	// behavior exactly.
	Lock surfaces.Locker

	broker *broker
}

// MCPPort is the seam between this surface and internal/surfaces/mcp — the
// same nil-valid-optional-interface idiom SkillSetPort/RunCtlPort/
// OversightPort already use.
type MCPPort interface {
	Resolve(ctx context.Context, tenantID, userID uuid.UUID) (schemas []provider.ToolSchema, digest []byte, err error)
}

// SpanEmitter is the seam between this surface and internal/obs's two
// Exporter implementations (stdout, OTLPExporter) — structurally identical
// to obs.Exporter/obs.OTLPExporter's own Emit method, declared locally so
// this package depends on the shape, not a concrete exporter type (this
// package already imports internal/obs directly for Grants, so this is a
// convenience seam, not a boundary-rule requirement).
type SpanEmitter interface {
	Emit(name string, attrs obs.Attrs) error
}

// SkillSetPort is the seam between this surface and internal/skills — the
// same nil-valid-optional-interface idiom RunCtlPort/OversightPort already
// use, so this package never imports internal/skills directly.
type SkillSetPort interface {
	Digest(ctx context.Context, tenantID uuid.UUID) ([]byte, error)
}

func NewServer(starter RunStarter, st *store.Store, ks *crypto.KeyStore, catalogManifestDigest []byte) *Server {
	return &Server{Starter: starter, Store: st, KeyStore: ks, CatalogManifestDigest: catalogManifestDigest, broker: newBroker()}
}

// Handler returns the http.Handler cmd/nexusd mounts. Every route is
// individually wrapped in authMiddleware (README task 13.1) — an
// unregistered path still gets the mux's own native 404 without needing a
// token first.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/runs", s.authed(s.handleCreateRun))
	mux.Handle("GET /v1/runs/{id}", s.authed(s.handleGetRun))
	mux.Handle("GET /v1/runs/{id}/events", s.authed(s.handleEvents))
	mux.Handle("GET /v1/sessions", s.authed(s.handleListSessions))
	if s.Oversight != nil {
		mux.Handle("GET /v1/approvals", s.authed(s.handleListApprovals))
		mux.Handle("GET /v1/approvals/{id}", s.authed(s.handleGetApproval))
		mux.Handle("POST /v1/approvals/{id}/grant", s.authed(s.handleGrantApproval))
		mux.Handle("POST /v1/approvals/{id}/deny", s.authed(s.handleDenyApproval))
	}
	if s.Grants != nil {
		mux.Handle("POST /v1/sessions/{id}/content-access-grants", s.authed(s.handleRequestContentAccessGrant))
		mux.Handle("GET /v1/sessions/{id}/content-access-grants/read", s.authed(s.handleReadUnderGrant))
	}
	if s.RunCtl != nil {
		mux.Handle("POST /v1/runs/{id}/cancel", s.authed(s.handleCancelRun))
		mux.Handle("POST /v1/runs/{id}/steer", s.authed(s.handleSteerRun))
		mux.Handle("POST /v1/runs/{id}/autonomy", s.authed(s.handleTightenAutonomy))
		mux.Handle("POST /v1/runs/{id}/fork", s.authed(s.handleForkRun))
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

// principal reads the calling identity authMiddleware already verified and
// stored on the request context. It writes the response itself on failure,
// mirroring the shape http.Error already uses, so call sites just check ok
// — in practice this only fails if authMiddleware somehow didn't run, since
// every route Handler() mounts is wrapped by it.
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (tenantID, userID uuid.UUID, ok bool) {
	tenantID, userID, ok = principalFromContext(r.Context())
	if !ok {
		http.Error(w, "no verified principal on request context", http.StatusUnauthorized)
		return uuid.Nil, uuid.Nil, false
	}
	return tenantID, userID, true
}
