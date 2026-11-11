// Package rest is the REST binding of the run use case: POST /v1/runs, GET
// /v1/runs/{id}, GET /v1/runs/{id}/events (SSE) and GET /v1/sessions. A thin
// translator only — it launches a run through a runs.Launcher, reads runs back
// through a runs.Reader, and forwards what comes back; it holds no agent control
// flow of its own (constitution Principle I) and never imports the kernel
// (tests/contract/boundaries_test.go).
//
// It also owns the live fan-out of a run's events: the in-process broker the SSE
// handler subscribes to, the Redis-backed bus that carries events between nexusd
// processes, and the two reactions to an event that must happen exactly once
// (the approval notification, the terminal span). cmd/nexusd calls PublishEvent
// and PublishDelta from the queue workers and the kernel's chunk callback.
//
// The authentication, server and route plumbing it mounts through are the kit in
// internal/surfaces/rest.
package rest

import (
	"context"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
)

// Handler holds everything the run endpoints need to admit, serve and stream
// runs over the runs.Launcher it wraps.
type Handler struct {
	// Launcher admits and starts a run (runs.Service in production): the one
	// definition of session admission, key provisioning and run start that
	// every surface shares. Mandatory: a nil Launcher fails handleCreateRun
	// closed (500) rather than panic.
	Launcher runs.Launcher
	// Reader is the read side of runs (runs.Reader): run rows and their
	// events, with payload decryption for the run's own audience. Mandatory,
	// like Launcher.
	Reader runs.Reader

	// Skills, if set, resolves one tenant's admitted-skill-set digest at
	// session-creation time (README task 7.6) — nil leaves
	// harness.Config.SkillSetDigest at its pre-Phase-7 zero value, which
	// every earlier caller and test still gets. Unlike CatalogManifestDigest
	// (fixed once at process startup — the resident tool catalog is
	// process-wide), a skill set is per-tenant, so this has to be resolved
	// per request rather than baked into a Handler field.
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

	// Bus fans a durably-appended event or a live delta out to every
	// nexusd process, not just this one (broker.go's own doc comment) — the
	// companion this surface needs now that a run's turn loop may execute
	// on a different process entirely (internal/queue's worker pool)
	// than whichever process holds the client's own SSE connection. Nil is
	// valid (every pre-this-change test, and any single-process dev run)
	// and simply means every publish stays local to this process's own
	// in-memory broker — exactly today's behavior.
	Bus EventBus

	broker *broker
}

var _ restkit.Module = (*Handler)(nil)

// EventBus is the minimal publish surface broker.go/run_create.go need —
// cmd/nexusd wires a *redis.Client-backed implementation (Redis Pub/Sub, a
// single Publish call) and runs the matching subscriber loop
// (startEventBus) that turns a remote publish back into a local
// broker.publish/closeSession call, so run_events.go's own merge-by-seq SSE
// logic needs zero changes regardless of which process actually produced
// the event.
type EventBus interface {
	Publish(ctx context.Context, channel string, payload []byte) error
}

// MCPPort is the seam between the run endpoints and internal/surfaces/mcp —
// the same nil-valid-optional-interface idiom SkillSetPort uses.
type MCPPort interface {
	Resolve(ctx context.Context, tenantID, userID uuid.UUID) (schemas []provider.ToolSchema, digest []byte, err error)
}

// SpanEmitter is the seam between the run endpoints and internal/obs's two
// Exporter implementations (stdout, OTLPExporter) — structurally identical
// to obs.Exporter/obs.OTLPExporter's own Emit method, declared locally so
// this package depends on the shape, not a concrete exporter type.
type SpanEmitter interface {
	Emit(name string, attrs obs.Attrs) error
}

// SkillSetPort is the seam between the run endpoints and internal/skills — the
// same nil-valid-optional-interface idiom MCPPort uses, so this package never
// imports internal/skills directly.
type SkillSetPort interface {
	Digest(ctx context.Context, tenantID uuid.UUID) ([]byte, error)
}

// NewHandler returns a Handler over the launcher and reader every surface
// shares.
func NewHandler(launcher runs.Launcher, reader runs.Reader) *Handler {
	return &Handler{Launcher: launcher, Reader: reader, broker: newBroker()}
}

// Routes implements restkit.Module.
func (h *Handler) Routes(r restkit.Router) {
	r.POST("/v1/runs", h.handleCreateRun)
	r.GET("/v1/runs/:id", h.handleGetRun)
	r.GET("/v1/runs/:id/events", h.handleEvents)
	r.GET("/v1/sessions", h.handleListSessions)
}
