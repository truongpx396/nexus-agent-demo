package rest

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

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
