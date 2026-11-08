package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/runs"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
)

type getRunResponse struct {
	RunID          string  `json:"run_id"`
	Status         string  `json:"status"`
	TerminalReason *string `json:"terminal_reason,omitempty"`
}

func (h *Handler) handleGetRun(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := restkit.Principal(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}

	run, ok := h.ownedRun(w, r, tenantID, userID, id)
	if !ok {
		return
	}

	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(getRunResponse{RunID: run.RunID.String(), Status: run.Status, TerminalReason: run.TerminalReason})
}

// ownedRun loads run id for the calling principal, writing the error response
// itself when it cannot: 404 for a run that does not exist or is not visible to
// the tenant, 403 for one that belongs to another user, 500 for a real read
// failure. Reporting a failed read as "not found" would hide an outage behind
// a client error.
func (h *Handler) ownedRun(w http.ResponseWriter, r *http.Request, tenantID, userID, id uuid.UUID) (runs.RunView, bool) {
	run, err := h.getSession(r.Context(), tenantID, id)
	switch {
	case errors.Is(err, runs.ErrNotFound):
		http.Error(w, "run not found", http.StatusNotFound)
		return runs.RunView{}, false
	case err != nil:
		http.Error(w, "load run: "+err.Error(), http.StatusInternalServerError)
		return runs.RunView{}, false
	case run.UserID != userID:
		http.Error(w, "forbidden", http.StatusForbidden)
		return runs.RunView{}, false
	}
	return run, true
}

func (h *Handler) getSession(ctx context.Context, tenantID, sessionID uuid.UUID) (runs.RunView, error) {
	return h.Reader.GetRun(ctx, tenantID, sessionID)
}
