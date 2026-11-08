// Package rest is the REST binding of run control: cancel a run, steer it,
// tighten its autonomy, or fork it at an earlier point (README tasks 6.9-6.11).
// A thin translator over internal/runctl: whether an operation is allowed in the
// session's current state, and what it appends to the log, are runctl's.
//
// The routes mount through the kit in internal/surfaces/rest.
package rest

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
)

// Controller is the part of runctl.Control the endpoints drive.
type Controller interface {
	Cancel(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) error
	// Send delivers a human's message: it continues a conversation paused in
	// awaiting_input and steers anything else (runctl.Control.Send).
	Send(ctx context.Context, tenantID, sessionID uuid.UUID, input string) error
	TightenAutonomy(ctx context.Context, tenantID, sessionID uuid.UUID, target string) error
	Fork(ctx context.Context, tenantID, parentSessionID uuid.UUID, atSeq int64, overrides runctl.ForkOverrides) (runctl.ForkResult, error)
}

// Handler serves the run-control endpoints. Control is mandatory.
type Handler struct {
	Control Controller
}

var _ restkit.Module = Handler{}

// Routes implements restkit.Module.
func (h Handler) Routes(r restkit.Router) {
	r.Handle("POST /v1/runs/{id}/cancel", h.handleCancelRun)
	r.Handle("POST /v1/runs/{id}/steer", h.handleSteerRun)
	r.Handle("POST /v1/runs/{id}/autonomy", h.handleTightenAutonomy)
	r.Handle("POST /v1/runs/{id}/fork", h.handleForkRun)
}

// ForkView is what this surface exposes for one fork — translated from
// runctl.ForkResult. DigestDiverged is surfaced explicitly and first — task
// 6.11's own "reports digest divergence rather than presenting it as a
// reproduction" is a caller-facing guarantee, not just an internal log line.
type ForkView struct {
	SessionID      string `json:"session_id"`
	DigestDiverged bool   `json:"digest_diverged"`
	ParentDigest   string `json:"parent_digest_hex"`
	ChildDigest    string `json:"child_digest_hex"`
}

type cancelRequest struct {
	Reason string `json:"reason"`
}

func (h Handler) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := restkit.Principal(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}
	var req cancelRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	if err := h.Control.Cancel(r.Context(), tenantID, id, req.Reason); err != nil {
		http.Error(w, "cancel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	restkit.WriteJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
}

type steerRequest struct {
	Input string `json:"input"`
}

// handleSteerRun delivers a message to a run. A conversational session paused in
// awaiting_input isn't mid-run, so there is nothing to steer: Send continues it
// instead, durably appending the message and queueing the continuation for a
// worker. An SSE connection that has been open since the run first paused (it
// never saw a terminal event) picks the continuation up live over the same
// Redis-backed event bus every other run publishes through, whichever process's
// worker actually resumes it.
func (h Handler) handleSteerRun(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := restkit.Principal(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}
	var req steerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := h.Control.Send(r.Context(), tenantID, id, req.Input); err != nil {
		http.Error(w, "steer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	restkit.WriteJSON(w, http.StatusOK, map[string]string{"status": "steered"})
}

type tightenAutonomyRequest struct {
	Target string `json:"target"`
}

func (h Handler) handleTightenAutonomy(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := restkit.Principal(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}
	var req tightenAutonomyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := h.Control.TightenAutonomy(r.Context(), tenantID, id, req.Target); err != nil {
		http.Error(w, "tighten autonomy: "+err.Error(), http.StatusBadRequest)
		return
	}
	restkit.WriteJSON(w, http.StatusOK, map[string]string{"status": "tightened", "autonomy_level": req.Target})
}

type forkRequest struct {
	AtSeq int64  `json:"at_seq"`
	Model string `json:"model,omitempty"`
}

func (h Handler) handleForkRun(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := restkit.Principal(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}
	var req forkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	result, err := h.Control.Fork(r.Context(), tenantID, id, req.AtSeq, runctl.ForkOverrides{ModelID: req.Model})
	if err != nil {
		http.Error(w, "fork: "+err.Error(), http.StatusInternalServerError)
		return
	}
	restkit.WriteJSON(w, http.StatusOK, ForkView{
		SessionID: result.SessionID.String(), DigestDiverged: result.DigestDiverged,
		ParentDigest: hex.EncodeToString(result.ParentDigest), ChildDigest: hex.EncodeToString(result.ChildDigest),
	})
}
