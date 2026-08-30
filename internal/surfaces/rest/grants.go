package rest

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Content access grants (README task 5.11) — the only path to plaintext
// outside a run's own audience. Distinct from handleEvents above (this
// file's sibling), which is the run's OWN owning user reading their own
// session's content (pattern #51, gated on sess.UserID == userID): here the
// requester and the grantee are independent principals, and every grant AND
// every read produces its own audited receipt (internal/obs.Grants).

type requestGrantRequest struct {
	GranteeID  string `json:"grantee_id"`
	Reason     string `json:"reason"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

type grantView struct {
	GrantID   string    `json:"grant_id"`
	SessionID string    `json:"session_id"`
	GranteeID string    `json:"grantee_id"`
	Reason    string    `json:"reason"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) handleRequestContentAccessGrant(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := s.principal(w, r)
	if !ok {
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}
	var req requestGrantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	granteeID, err := uuid.Parse(req.GranteeID)
	if err != nil {
		http.Error(w, `"grantee_id" is invalid`, http.StatusBadRequest)
		return
	}
	if req.Reason == "" {
		http.Error(w, `"reason" is required`, http.StatusBadRequest)
		return
	}

	gr, err := s.Grants.RequestGrant(r.Context(), tenantID, sessionID, granteeID, req.Reason, time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		http.Error(w, "request grant: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, grantView{
		GrantID: gr.GrantID.String(), SessionID: gr.SessionID.String(), GranteeID: gr.GranteeID.String(),
		Reason: gr.Reason, ExpiresAt: gr.ExpiresAt,
	})
}

func (s *Server) handleReadUnderGrant(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := s.principal(w, r)
	if !ok {
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}

	// The calling principal IS the grantee here — unlike
	// handleRequestContentAccessGrant, where an admin names a DIFFERENT
	// principal to authorize, a read is always "show ME what I was
	// granted," never "show me what someone else was granted."
	dtos, err := s.Grants.Read(r.Context(), tenantID, sessionID, userID)
	if err != nil {
		http.Error(w, "read under grant: "+err.Error(), http.StatusForbidden)
		return
	}
	writeJSON(w, http.StatusOK, dtos)
}
