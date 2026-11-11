package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
)

// CreateRun is POST /v1/runs. The request is api/openapi.yaml's
// CreateRunRequest: BudgetUSD is parsed by cost.ParseDecimal (never a binary
// float) into a session-scoped budgets row created alongside the session, and
// empty means no session-level ceiling; Conversational opts the session into
// pause-not-terminate semantics (runs.LaunchRequest.Conversational), so a plain
// reply suspends it awaiting the next message and POST .../steer resumes it in
// place.
func (h *Handler) CreateRun(c *gin.Context) {
	// Task 7.13: the turn-submitting principal, resolved fresh from THIS
	// request — ResolvePrincipal reads the same verified principal Principal
	// does, typed as capability.Principal, since starting a run is the clearest
	// "submitting one turn" action this surface has.
	principal, perr := restkit.ResolvePrincipal(c)
	if perr != nil {
		restkit.Error(c, perr.Error(), http.StatusUnauthorized)
		return
	}
	tenantID, userID := principal.TenantID, principal.UserID
	ctx := c.Request.Context()

	var req CreateRunRequest
	if !restkit.DecodeJSON(c, &req) {
		return
	}
	if req.Input == "" {
		restkit.Error(c, `"input" is required`, http.StatusBadRequest)
		return
	}
	// Empty defaults to "supervised", matching store.CreateSession's own default.
	autonomy := req.Autonomy
	if autonomy == "" {
		autonomy = CreateRunRequestAutonomySupervised
	}
	if !autonomy.Valid() {
		restkit.Error(c, `"autonomy" must be one of "read_only", "supervised", "autonomous"`, http.StatusBadRequest)
		return
	}

	var skillSetDigest []byte
	if h.Skills != nil {
		var derr error
		skillSetDigest, derr = h.Skills.Digest(ctx, tenantID)
		if derr != nil {
			restkit.ServerError(c, "resolve skill set digest", derr)
			return
		}
	}

	// Task 11.1: resolved as THIS request's own userID (an oauth_connector-
	// authed MCP server authenticates as the calling user, never a service
	// account) — one call gets both the extra model-visible catalog
	// entries and the digest folded into harness_digest below, so the two
	// can never silently disagree (mcp.Port.Resolve's own doc comment).
	var mcpCatalog []provider.ToolSchema
	var mcpCatalogDigest []byte
	if h.MCP != nil {
		var derr error
		mcpCatalog, mcpCatalogDigest, derr = h.MCP.Resolve(ctx, tenantID, userID)
		if derr != nil {
			restkit.ServerError(c, "resolve MCP catalog", derr)
			return
		}
	}

	if h.Launcher == nil {
		restkit.Error(c, "server misconfigured: no run launcher wired", http.StatusInternalServerError)
		return
	}
	launched, err := h.Launcher.Launch(ctx, runs.LaunchRequest{
		TenantID: tenantID, UserID: userID, SurfaceID: "rest", Input: req.Input,
		DataLabel: provider.DataLabel(req.DataLabel), Difficulty: provider.Difficulty(req.Difficulty), Autonomy: string(autonomy),
		BudgetUSD: req.BudgetUSD, Conversational: req.Conversational,
		SkillSetDigest: skillSetDigest, MCPCatalogDigest: mcpCatalogDigest, MCPCatalog: mcpCatalog,
	})
	var notAdmitted *runs.NotAdmittedError
	switch {
	case errors.As(err, &notAdmitted):
		restkit.Error(c, "create run: "+notAdmitted.Reason, http.StatusBadRequest)
		return
	case err != nil:
		restkit.ServerError(c, "create run", err)
		return
	}
	go h.publishUntilDone(tenantID, launched.SessionID, launched.Events)

	c.JSON(http.StatusAccepted, CreateRunResponse{RunID: launched.SessionID})
}

// publishUntilDone drains the runs.Starter's event channel, publishing each
// event via h.PublishEvent (broker.go) — the same call cmd/nexusd's
// queueRunner.Run makes for a queued job's own events, so this package's
// synchronous fast path and a queued run driven by an entirely different
// process converge on identical delivery/side-effect handling. It no longer
// closes the session's subscribers itself when the channel closes: a
// queued run's own channel (kernelRunStarter.StartRun) now closes as soon
// as its synchronous Kernel.Seed step finishes, long before the run's real
// terminal event exists — closing is RelayEvent's job now, triggered by
// observing an actual EventTerminal, wherever (and whenever) it's actually
// published from.
func (h *Handler) publishUntilDone(tenantID, sessionID uuid.UUID, events <-chan runs.Event) {
	for re := range events {
		h.PublishEvent(tenantID, sessionID, re.Event, re.Err)
	}
}

// emitTerminalSpan is README task 13.12's one concrete span-emission call
// site: one content-free "run.terminal" span per completed run —
// session.id/tenant.id/terminal_reason, the same three fields
// Run already exposes over plain HTTP, now also reaching
// whichever Exporter (stdout or OTLPExporter, internal/obs) cmd/nexusd
// wired in. TerminalReason is read from the session row (a plaintext
// column, eventlog.Session's own field — never the sealed event payload), so
// this needs no decrypt path of its own.
func (h *Handler) emitTerminalSpan(tenantID, sessionID uuid.UUID) {
	sess, err := h.getSession(context.Background(), tenantID, sessionID)
	if err != nil {
		log.Error().Err(err).Any("session_id", sessionID).Msg("rest: emit terminal span: load session")
		return
	}
	attrs := obs.Attrs{"session.id": sessionID.String(), "tenant.id": tenantID.String()}
	if sess.TerminalReason != nil {
		attrs["terminal_reason"] = *sess.TerminalReason
	}
	if err := h.Exporter.Emit("run.terminal", attrs); err != nil {
		log.Error().Err(err).Any("session_id", sessionID).Msg("rest: emit terminal span")
	}
}

// deliverApprovalNotification is task 7.14's one real call site: an
// approval_requested event durably needs a human to see it, so it goes
// through the outbox's at-least-once discipline rather than only the
// broker's best-effort SSE fan-out. The notification payload is
// deliberately minimal (session id + tool id, both already-plaintext
// structural fields on eventlog.Event) — never the event's own sealed
// payload, which this package has no decrypt path for anyway.
func (h *Handler) deliverApprovalNotification(sessionID uuid.UUID, ev eventlog.Event) {
	toolID := ""
	if ev.ToolID != nil {
		toolID = *ev.ToolID
	}
	payload, err := json.Marshal(map[string]string{"session_id": sessionID.String(), "tool_id": toolID})
	if err != nil {
		log.Error().Err(err).Any("session_id", sessionID).Msg("rest: marshal approval notification payload")
		return
	}
	const operatorRecipient = "operator" // no per-tenant notification-target config exists yet (Phase 11's connector/OAuth work); one fixed recipient is the honest interim
	if err := h.Outbox.Deliver(context.Background(), ev.TenantID, sessionID, ev.Seq, restkit.Descriptor.SurfaceID, operatorRecipient, payload, h.OutboxSender); err != nil {
		log.Error().Err(err).Any("session_id", sessionID).Any("seq", ev.Seq).Msg("rest: deliver approval notification")
	}
}
