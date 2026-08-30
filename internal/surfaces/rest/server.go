// Package rest is the REST surface (README task 2.10): POST /v1/runs, GET
// /v1/runs/{id}, GET /v1/runs/{id}/events. A thin translator only — it
// creates a session, hands it to a RunStarter (starter.go), and forwards
// what comes back; it holds no agent control flow of its own (constitution
// Principle I) and never imports the kernel package directly
// (tests/contract/boundaries_test.go).
//
// AuthN is a dev stand-in: the calling principal is read from
// X-Nexus-Tenant-ID / X-Nexus-User-ID headers rather than verified via a
// real per-tenant OIDC flow. Real control-plane AuthN isn't a task in
// Phase 1 or Phase 2's list — this is documented scope, not an oversight.
package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/harness"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Server holds everything one REST process needs to admit and serve runs
// over the RunStarter it wraps.
type Server struct {
	Starter  RunStarter
	Store    *store.Store
	KeyStore *crypto.KeyStore

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

	broker *broker
}

func NewServer(starter RunStarter, st *store.Store, ks *crypto.KeyStore, catalogManifestDigest []byte) *Server {
	return &Server{Starter: starter, Store: st, KeyStore: ks, CatalogManifestDigest: catalogManifestDigest, broker: newBroker()}
}

// Handler returns the http.Handler cmd/nexusd mounts.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runs", s.handleCreateRun)
	mux.HandleFunc("GET /v1/runs/{id}", s.handleGetRun)
	mux.HandleFunc("GET /v1/runs/{id}/events", s.handleEvents)
	if s.Oversight != nil {
		mux.HandleFunc("GET /v1/approvals", s.handleListApprovals)
		mux.HandleFunc("GET /v1/approvals/{id}", s.handleGetApproval)
		mux.HandleFunc("POST /v1/approvals/{id}/grant", s.handleGrantApproval)
		mux.HandleFunc("POST /v1/approvals/{id}/deny", s.handleDenyApproval)
	}
	if s.Grants != nil {
		mux.HandleFunc("POST /v1/sessions/{id}/content-access-grants", s.handleRequestContentAccessGrant)
		mux.HandleFunc("GET /v1/sessions/{id}/content-access-grants/read", s.handleReadUnderGrant)
	}
	if s.RunCtl != nil {
		mux.HandleFunc("POST /v1/runs/{id}/cancel", s.handleCancelRun)
		mux.HandleFunc("POST /v1/runs/{id}/steer", s.handleSteerRun)
		mux.HandleFunc("POST /v1/runs/{id}/autonomy", s.handleTightenAutonomy)
		mux.HandleFunc("POST /v1/runs/{id}/fork", s.handleForkRun)
	}
	return mux
}

// principal reads the dev-mode calling identity off the request. It writes
// the response itself on failure, mirroring the shape http.Error already
// uses, so call sites just check ok.
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (tenantID, userID uuid.UUID, ok bool) {
	tenantID, err := uuid.Parse(r.Header.Get("X-Nexus-Tenant-ID"))
	if err != nil {
		http.Error(w, "missing or invalid X-Nexus-Tenant-ID header", http.StatusUnauthorized)
		return uuid.Nil, uuid.Nil, false
	}
	userID, err = uuid.Parse(r.Header.Get("X-Nexus-User-ID"))
	if err != nil {
		http.Error(w, "missing or invalid X-Nexus-User-ID header", http.StatusUnauthorized)
		return uuid.Nil, uuid.Nil, false
	}
	return tenantID, userID, true
}

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
	tenantID, userID, ok := s.principal(w, r)
	if !ok {
		return
	}

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

	var budgetCeiling *cost.Money
	if req.BudgetUSD != "" {
		amount, perr := cost.ParseDecimal(req.BudgetUSD, cost.DefaultCurrency)
		if perr != nil {
			http.Error(w, `"budget_usd" is invalid: `+perr.Error(), http.StatusBadRequest)
			return
		}
		budgetCeiling = &amount
	}

	sessionID := uuid.New()
	digest := harness.Digest(harness.Config{
		SystemPromptVersion:   "phase2-v1",
		CatalogManifestDigest: s.CatalogManifestDigest,
		PromptMode:            "phase2-single-shot",
	})

	var dek crypto.DEK
	err := s.Store.InTenantTx(r.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		dek, derr = s.KeyStore.NewDEK(ctx, tx, tenantID)
		if derr != nil {
			return derr
		}
		if err := store.CreateSession(ctx, tx, store.Session{
			SessionID:     sessionID,
			SessionKey:    sessionID.String(),
			TenantID:      tenantID,
			SurfaceID:     "rest",
			UserID:        userID,
			AgentID:       uuid.Nil, // no agent registry yet (Phase 3+); a fresh run has no config row to pin to
			AgentVersion:  1,
			HarnessDigest: digest,
			DataLabel:     string(dataLabel),
			RouteModelID:  route.ModelID,
			RouteReason:   route.Reason,
			AutonomyLevel: autonomy,
		}); err != nil {
			return err
		}
		if budgetCeiling != nil {
			// A session-scoped budget (internal/cost, Phase 4 task 4.5) —
			// created in the SAME transaction as the session row it
			// references (budgets.scope_ref has an FK to sessions), so a
			// caller never observes a session with no way to enforce the
			// ceiling it asked for.
			if _, err := cost.CreateBudget(ctx, tx, tenantID, cost.BudgetScopeSession, &sessionID, *budgetCeiling); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		http.Error(w, "create run: "+err.Error(), http.StatusInternalServerError)
		return
	}

	req2 := RunRequest{
		SessionID:     sessionID,
		TenantID:      tenantID,
		Seal:          sealFuncFor(dek, tenantID, sessionID),
		Input:         req.Input,
		ModelID:       route.ModelID,
		AutonomyLevel: autonomy,
	}
	events, err := s.Starter.StartRun(context.Background(), req2) // a run outlives the HTTP request that started it
	if err != nil {
		http.Error(w, "start run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	go s.publishUntilDone(sessionID, events)

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
func (s *Server) publishUntilDone(sessionID uuid.UUID, events <-chan RunEvent) {
	defer s.broker.closeSession(sessionID)
	for re := range events {
		s.broker.publish(sessionID, published(re))
	}
}

type getRunResponse struct {
	RunID          string  `json:"run_id"`
	Status         string  `json:"status"`
	TerminalReason *string `json:"terminal_reason,omitempty"`
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := s.principal(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}

	sess, err := s.getSession(r.Context(), tenantID, id)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	if sess.UserID != userID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(getRunResponse{RunID: sess.SessionID.String(), Status: sess.Status, TerminalReason: sess.TerminalReason})
}

func (s *Server) getSession(ctx context.Context, tenantID, sessionID uuid.UUID) (store.Session, error) {
	var sess store.Session
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var gerr error
		sess, gerr = store.GetSession(ctx, tx, sessionID)
		return gerr
	})
	return sess, err
}

// eventDTO is what an SSE frame's data carries: enough structure to render
// a client-side view, plus Body — the decrypted payload for the run's own
// audience (constitution: "audience-gated run output" is a distinct signal
// class from content-free telemetry; internal/obs never carries this).
// EventThought never gets a Body: reasoning is "round-tripped, never shown"
// (internal/provider's doc comment) even to the run's own audience.
type eventDTO struct {
	EventID   string          `json:"event_id"`
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Actor     string          `json:"actor"`
	ToolID    *string         `json:"tool_id,omitempty"`
	PairRef   *string         `json:"pair_ref,omitempty"`
	ModelID   *string         `json:"model_id,omitempty"`
	CreatedAt string          `json:"created_at"`
	Body      json.RawMessage `json:"body,omitempty"`
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := s.principal(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}
	sess, err := s.getSession(r.Context(), tenantID, id)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	if sess.UserID != userID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Subscribe BEFORE replaying history, so nothing published between the
	// subscribe call and the replay finishing is missed. That ordering, on
	// its own, can instead deliver an event TWICE (once via the replay,
	// once live) if it's appended in the window between subscribing and
	// finishing the replay — closed below by tracking the highest seq the
	// replay actually sent and discarding anything from the live channel at
	// or below it: seq is strictly sequential per session (store.Append),
	// so "already replayed" is exactly "seq <= lastSeq", no gaps possible.
	ch, unsubscribe := s.broker.subscribe(id)
	defer unsubscribe()

	dekCache := map[string]crypto.DEK{}
	write := func(e store.Event) bool {
		dto, derr := s.toEventDTO(r.Context(), tenantID, e, dekCache)
		if derr != nil {
			writeSSEFrame(w, "error", map[string]string{"error": derr.Error()})
			flusher.Flush()
			return false
		}
		writeSSEFrame(w, string(e.Type), dto)
		flusher.Flush()
		return e.Type != store.EventTerminal
	}

	history, err := s.listEvents(r.Context(), tenantID, id)
	if err != nil {
		writeSSEFrame(w, "error", map[string]string{"error": err.Error()})
		flusher.Flush()
		return
	}
	var lastSeq int64
	for _, e := range history {
		if !write(e) {
			return
		}
		lastSeq = e.Seq
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case p, chOK := <-ch:
			if !chOK {
				return
			}
			if p.Err == nil && p.Event.Seq <= lastSeq {
				continue // already delivered by the historical replay above
			}
			if p.Err != nil {
				writeSSEFrame(w, "error", map[string]string{"error": p.Err.Error()})
				flusher.Flush()
				return
			}
			if !write(p.Event) {
				return
			}
		}
	}
}

func (s *Server) listEvents(ctx context.Context, tenantID, sessionID uuid.UUID) ([]store.Event, error) {
	var events []store.Event
	err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var lerr error
		events, lerr = store.ListEvents(ctx, tx, sessionID)
		return lerr
	})
	return events, err
}

// toEventDTO decrypts one event's payload for display to the run's own
// audience (the principal check in handleEvents has already established
// this request IS that audience). dekCache avoids re-unwrapping the same
// key (in practice, every event in one session shares it) once per event.
func (s *Server) toEventDTO(ctx context.Context, tenantID uuid.UUID, e store.Event, dekCache map[string]crypto.DEK) (eventDTO, error) {
	dto := eventDTO{
		EventID:   e.EventID.String(),
		Seq:       e.Seq,
		Type:      string(e.Type),
		Actor:     string(e.Actor),
		ToolID:    e.ToolID,
		ModelID:   e.ModelID,
		CreatedAt: e.CreatedAt.Format(time.RFC3339Nano),
	}
	if e.PairRef != nil {
		ref := e.PairRef.String()
		dto.PairRef = &ref
	}
	if e.Type == store.EventThought {
		return dto, nil // never shown, even to the run's own audience
	}

	dek, ok := dekCache[e.KeyID]
	if !ok {
		var uerr error
		err := s.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			dek, uerr = s.KeyStore.Unwrap(ctx, tx, e.KeyID)
			return uerr
		})
		if err != nil {
			return eventDTO{}, fmt.Errorf("unwrap key for event %s: %w", e.EventID, err)
		}
		dekCache[e.KeyID] = dek
	}

	plaintext, err := crypto.Open(dek, e.Payload, tenantID.String(), e.SessionID.String())
	if err != nil {
		return eventDTO{}, fmt.Errorf("decrypt event %s: %w", e.EventID, err)
	}
	dto.Body = json.RawMessage(plaintext)
	return dto, nil
}

func writeSSEFrame(w http.ResponseWriter, event string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		raw = []byte(`{"error":"failed to marshal event"}`)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw) // best-effort: a broken client connection is discovered on the next Flush, not here
}
