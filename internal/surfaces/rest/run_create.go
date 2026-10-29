package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
)

type createRunRequest struct {
	Input      string `json:"input"`
	DataLabel  string `json:"data_label,omitempty"`
	Difficulty string `json:"difficulty,omitempty"`
	// Autonomy pins the session's permission-chain autonomy level (Phase 3,
	// internal/permissions/autonomy.go): "read_only" | "supervised" |
	// "autonomous". Empty defaults to "supervised", matching
	// store.CreateSession's own default.
	Autonomy string `json:"autonomy,omitempty"`
	// BudgetUSD, if set, is a per-task hard ceiling in decimal USD (e.g.
	// "0.05") — internal/cost, Phase 4, task 4.5's worker-local, per-run
	// ceiling. Parsed via cost.ParseDecimal (never a binary float) into a
	// session-scoped budgets row created alongside the session itself.
	// Empty means no session-level ceiling — cost.Gate.Reserve resolves
	// DecisionSkip for this leg (a tenant-scoped ceiling, if configured
	// out of band, still applies).
	BudgetUSD string `json:"budget_usd,omitempty"`
	// Conversational opts this session into pause-not-terminate semantics
	// (runs.Request.Conversational's own doc comment, kernel.RunConfig.
	// Conversational's underlying one) — a plain reply suspends the
	// session awaiting the caller's next message instead of ending the
	// run, and POST .../steer transparently resumes it in place rather
	// than the caller needing to start a new session. Omitted/false
	// reproduces today's one-task-per-session behavior exactly; the web
	// chat UI is the first caller to set it true.
	Conversational bool `json:"conversational,omitempty"`
}

type createRunResponse struct {
	RunID string `json:"run_id"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	// Task 7.13: the turn-submitting principal, resolved fresh from THIS
	// request — resolvePrincipal reads the same headers principal() does,
	// typed as capability.Principal, since starting a run is the clearest
	// "submitting one turn" action this surface has.
	principal, perr := s.resolvePrincipal(r)
	if perr != nil {
		http.Error(w, perr.Error(), http.StatusUnauthorized)
		return
	}
	tenantID, userID := principal.TenantID, principal.UserID

	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Input == "" {
		http.Error(w, `"input" is required`, http.StatusBadRequest)
		return
	}
	autonomy := req.Autonomy
	switch autonomy {
	case "":
		autonomy = "supervised"
	case "read_only", "supervised", "autonomous":
		// valid
	default:
		http.Error(w, `"autonomy" must be one of "read_only", "supervised", "autonomous"`, http.StatusBadRequest)
		return
	}

	var skillSetDigest []byte
	if s.Skills != nil {
		var derr error
		skillSetDigest, derr = s.Skills.Digest(r.Context(), tenantID)
		if derr != nil {
			http.Error(w, "resolve skill set digest: "+derr.Error(), http.StatusInternalServerError)
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
	if s.MCP != nil {
		var derr error
		mcpCatalog, mcpCatalogDigest, derr = s.MCP.Resolve(r.Context(), tenantID, userID)
		if derr != nil {
			http.Error(w, "resolve MCP catalog: "+derr.Error(), http.StatusInternalServerError)
			return
		}
	}

	if s.Launcher == nil {
		http.Error(w, "server misconfigured: no run launcher wired", http.StatusInternalServerError)
		return
	}
	launched, err := s.Launcher.Launch(r.Context(), runs.LaunchRequest{
		TenantID: tenantID, UserID: userID, SurfaceID: "rest", Input: req.Input,
		DataLabel: provider.DataLabel(req.DataLabel), Difficulty: provider.Difficulty(req.Difficulty), Autonomy: autonomy,
		BudgetUSD: req.BudgetUSD, Conversational: req.Conversational,
		SkillSetDigest: skillSetDigest, MCPCatalogDigest: mcpCatalogDigest, MCPCatalog: mcpCatalog,
	})
	var notAdmitted *runs.NotAdmittedError
	switch {
	case errors.As(err, &notAdmitted):
		http.Error(w, "create run: "+notAdmitted.Reason, http.StatusBadRequest)
		return
	case err != nil:
		http.Error(w, "create run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	go s.publishUntilDone(tenantID, launched.SessionID, launched.Events)

	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(createRunResponse{RunID: launched.SessionID.String()})
}

// publishUntilDone drains the runs.Starter's event channel, publishing each
// event via s.PublishEvent (broker.go) — the same call cmd/nexusd's
// queueRunner.Run makes for a queued job's own events, so this package's
// synchronous fast path and a queued run driven by an entirely different
// process converge on identical delivery/side-effect handling. It no longer
// closes the session's subscribers itself when the channel closes: a
// queued run's own channel (kernelRunStarter.StartRun) now closes as soon
// as its synchronous Kernel.Seed step finishes, long before the run's real
// terminal event exists — closing is RelayEvent's job now, triggered by
// observing an actual EventTerminal, wherever (and whenever) it's actually
// published from.
func (s *Server) publishUntilDone(tenantID, sessionID uuid.UUID, events <-chan runs.Event) {
	for re := range events {
		s.PublishEvent(tenantID, sessionID, re.Event, re.Err)
	}
}

// emitTerminalSpan is README task 13.12's one concrete span-emission call
// site: one content-free "run.terminal" span per completed run —
// session.id/tenant.id/terminal_reason, the same three fields
// getRunResponse already exposes over plain HTTP, now also reaching
// whichever Exporter (stdout or OTLPExporter, internal/obs) cmd/nexusd
// wired in. TerminalReason is read from the session row (a plaintext
// column, store.Session's own field — never the sealed event payload), so
// this needs no decrypt path of its own.
func (s *Server) emitTerminalSpan(tenantID, sessionID uuid.UUID) {
	sess, err := s.getSession(context.Background(), tenantID, sessionID)
	if err != nil {
		log.Error().Err(err).Any("session_id", sessionID).Msg("rest: emit terminal span: load session")
		return
	}
	attrs := obs.Attrs{"session.id": sessionID.String(), "tenant.id": tenantID.String()}
	if sess.TerminalReason != nil {
		attrs["terminal_reason"] = *sess.TerminalReason
	}
	if err := s.Exporter.Emit("run.terminal", attrs); err != nil {
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
func (s *Server) deliverApprovalNotification(sessionID uuid.UUID, ev eventlog.Event) {
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
	if err := s.Outbox.Deliver(context.Background(), ev.TenantID, sessionID, ev.Seq, Descriptor.SurfaceID, operatorRecipient, payload, s.OutboxSender); err != nil {
		log.Error().Err(err).Any("session_id", sessionID).Any("seq", ev.Seq).Msg("rest: deliver approval notification")
	}
}
