// Package rest is the REST binding of content-access grants (README task 5.11):
// request a grant, and read a session's content under one. A thin translator
// over internal/contentaccess, which decides who may read what and records an
// audited receipt for every grant and every read.
//
// The routes mount through the kit in internal/surfaces/rest.
package rest

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/contentaccess"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
)

// Handler serves the grant endpoints. Grants is mandatory.
type Handler struct {
	Grants *contentaccess.Grants
}

var _ restkit.Module = Handler{}

// Routes implements restkit.Module.
func (h Handler) Routes(r restkit.Router) {
	r.Handle("POST /v1/sessions/{id}/content-access-grants", h.handleRequestContentAccessGrant)
	r.Handle("GET /v1/sessions/{id}/content-access-grants/read", h.handleReadUnderGrant)
}

// Content access grants (README task 5.11) — the only path to plaintext
// outside a run's own audience. Distinct from handleEvents above (this
// file's sibling), which is the run's OWN owning user reading their own
// session's content (pattern #51, gated on sess.UserID == userID): here the
// requester and the grantee are independent principals, and every grant AND
// every read produces its own audited receipt (internal/contentaccess.Grants).

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

func (h Handler) handleRequestContentAccessGrant(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := restkit.Principal(w, r)
	if !ok {
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}
	var req requestGrantRequest
	if !restkit.DecodeJSON(w, r, &req) {
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

	gr, err := h.Grants.RequestGrant(r.Context(), tenantID, sessionID, granteeID, req.Reason, time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		restkit.ServerError(w, r, "request grant", err)
		return
	}
	restkit.WriteJSON(w, http.StatusCreated, grantView{
		GrantID: gr.GrantID.String(), SessionID: gr.SessionID.String(), GranteeID: gr.GranteeID.String(),
		Reason: gr.Reason, ExpiresAt: gr.ExpiresAt,
	})
}

func (h Handler) handleReadUnderGrant(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := restkit.Principal(w, r)
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
	dtos, err := h.Grants.Read(r.Context(), tenantID, sessionID, userID)
	if errors.Is(err, contentaccess.ErrNoGrant) {
		http.Error(w, "no valid content access grant", http.StatusForbidden)
		return
	}
	if err != nil {
		restkit.ServerError(w, r, "read under grant", err)
		return
	}
	restkit.WriteJSON(w, http.StatusOK, dtos)
}
