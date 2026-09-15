package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/controlplane"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/harness"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
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
	dataLabel := provider.DataLabel(req.DataLabel)
	if dataLabel == "" {
		dataLabel = provider.DataLabelInternal
	}
	difficulty := provider.Difficulty(req.Difficulty)
	if difficulty == "" {
		difficulty = provider.DifficultySimple
	}
	route := provider.Route(dataLabel, difficulty)

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

	sessionID := uuid.New()
	digest := harness.Digest(harness.Config{
		SystemPromptVersion:   "phase2-v1",
		CatalogManifestDigest: s.CatalogManifestDigest,
		SkillSetDigest:        skillSetDigest,
		PromptMode:            "phase2-single-shot",
		MCPCatalogDigest:      mcpCatalogDigest,
	})

	if s.ControlPlane == nil {
		http.Error(w, "server misconfigured: no control plane wired", http.StatusInternalServerError)
		return
	}

	// README task 13.15: session admission (the session row, plus an
	// optional session-scoped budget — internal/cost, Phase 4 task 4.5,
	// created together since budgets.scope_ref has an FK to sessions) goes
	// through internal/controlplane.Port — the versioned control-plane
	// boundary README §2 has always described. Encryption stays entirely
	// on this side of that boundary: the DEK is minted next, exactly as it
	// always has been, never inside AdmitRun (that package's own doc
	// comment on why).
	admitResult, err := s.ControlPlane.AdmitRun(r.Context(), controlplane.AdmitRunV1{
		TenantID: tenantID, UserID: userID, SessionID: sessionID, SurfaceID: "rest",
		DataLabel: string(dataLabel), RouteModelID: route.ModelID, RouteReason: route.Reason,
		Autonomy: autonomy, BudgetUSD: req.BudgetUSD, HarnessDigest: digest,
	})
	if err != nil {
		http.Error(w, "create run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !admitResult.Admitted {
		http.Error(w, "create run: "+admitResult.Reason, http.StatusBadRequest)
		return
	}

	var dek crypto.DEK
	err = s.Store.InTenantTx(r.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		dek, derr = s.KeyStore.NewDEK(ctx, tx, tenantID)
		return derr
	})
	if err != nil {
		http.Error(w, "create run: "+err.Error(), http.StatusInternalServerError)
		return
	}

	mcpLoadedTools := make([]string, len(mcpCatalog))
	for i, c := range mcpCatalog {
		mcpLoadedTools[i] = c.Name
	}
	req2 := RunRequest{
		SessionID:        sessionID,
		TenantID:         tenantID,
		Seal:             sealFuncFor(dek, tenantID, sessionID),
		Input:            req.Input,
		ModelID:          route.ModelID,
		AutonomyLevel:    autonomy,
		ExtraCatalog:     mcpCatalog,
		ExtraLoadedTools: mcpLoadedTools,
	}
	events, err := s.Starter.StartRun(context.Background(), req2) // a run outlives the HTTP request that started it
	if err != nil {
		http.Error(w, "start run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	go s.publishUntilDone(tenantID, sessionID, events)

	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(createRunResponse{RunID: sessionID.String()})
}

// sealFuncFor closes over the DEK and identifiers a SealFunc needs but
// doesn't itself carry.
func sealFuncFor(dek crypto.DEK, tenantID, sessionID uuid.UUID) SealFunc {
	return func(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", fmt.Errorf("seal event payload: %w", err)
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}
}

// publishUntilDone drains the RunStarter's event channel into the broker,
// closing the session's subscribers once the channel closes (the run has
// ended, normally or not) — this is the only place StartRun's result is
// consumed, so the broker's bookkeeping stays entirely inside this package.
func (s *Server) publishUntilDone(tenantID, sessionID uuid.UUID, events <-chan RunEvent) {
	defer s.broker.closeSession(sessionID)
	for re := range events {
		s.broker.publish(sessionID, published(re))
		if s.Outbox != nil && s.OutboxSender != nil && re.Err == nil && re.Event.Type == store.EventApprovalRequested {
			s.deliverApprovalNotification(sessionID, re.Event)
		}
		if s.Exporter != nil && re.Err == nil && re.Event.Type == store.EventTerminal {
			s.emitTerminalSpan(tenantID, sessionID)
		}
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
// structural fields on store.Event) — never the event's own sealed
// payload, which this package has no decrypt path for anyway.
func (s *Server) deliverApprovalNotification(sessionID uuid.UUID, ev store.Event) {
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
