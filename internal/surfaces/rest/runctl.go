package rest

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

// ForkView is what this surface exposes for one fork — translated from
// internal/runctl.ForkResult by whatever RunCtlPort implementation
// cmd/nexusd wires in. DigestDiverged is surfaced explicitly and first —
// task 6.11's own "reports digest divergence rather than presenting it as a
// reproduction" is a caller-facing guarantee, not just an internal log line.
type ForkView struct {
	SessionID      string `json:"session_id"`
	DigestDiverged bool   `json:"digest_diverged"`
	ParentDigest   string `json:"parent_digest_hex"`
	ChildDigest    string `json:"child_digest_hex"`
}

// RunCtlPort is the entire seam between this surface and internal/runctl —
// cmd/nexusd supplies the concrete implementation, the same exemption
// starter.go/oversight.go's own doc comments already grant for
// kernel.Kernel.Run/Resume: internal/runctl transitively imports kernel,
// which this package must never import directly
// (tests/contract/boundaries_test.go).
type RunCtlPort interface {
	Cancel(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) error
	Steer(ctx context.Context, tenantID, sessionID uuid.UUID, input string) error
	TightenAutonomy(ctx context.Context, tenantID, sessionID uuid.UUID, target string) error
	Fork(ctx context.Context, tenantID, sessionID uuid.UUID, atSeq int64, modelOverride string) (ForkView, error)
}

type cancelRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := s.principal(w, r)
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
	if err := s.RunCtl.Cancel(r.Context(), tenantID, id, req.Reason); err != nil {
		http.Error(w, "cancel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
}

type steerRequest struct {
	Input string `json:"input"`
}

func (s *Server) handleSteerRun(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := s.principal(w, r)
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
	if err := s.RunCtl.Steer(r.Context(), tenantID, id, req.Input); err != nil {
		http.Error(w, "steer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "steered"})
}

type tightenAutonomyRequest struct {
	Target string `json:"target"`
}

func (s *Server) handleTightenAutonomy(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := s.principal(w, r)
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
	if err := s.RunCtl.TightenAutonomy(r.Context(), tenantID, id, req.Target); err != nil {
		http.Error(w, "tighten autonomy: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "tightened", "autonomy_level": req.Target})
}

type forkRequest struct {
	AtSeq int64  `json:"at_seq"`
	Model string `json:"model,omitempty"`
}

func (s *Server) handleForkRun(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := s.principal(w, r)
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
	view, err := s.RunCtl.Fork(r.Context(), tenantID, id, req.AtSeq, req.Model)
	if err != nil {
		http.Error(w, "fork: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
