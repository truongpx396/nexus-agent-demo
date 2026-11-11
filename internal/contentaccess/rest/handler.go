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

	"github.com/gin-gonic/gin"
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
	r.POST("/v1/sessions/:id/content-access-grants", h.handleRequestContentAccessGrant)
	r.GET("/v1/sessions/:id/content-access-grants/read", h.handleReadUnderGrant)
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

func (h Handler) handleRequestContentAccessGrant(c *gin.Context) {
	tenantID, _, ok := restkit.Principal(c)
	if !ok {
		return
	}
	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		restkit.Error(c, "invalid session id", http.StatusBadRequest)
		return
	}
	var req requestGrantRequest
	if !restkit.DecodeJSON(c, &req) {
		return
	}
	granteeID, err := uuid.Parse(req.GranteeID)
	if err != nil {
		restkit.Error(c, `"grantee_id" is invalid`, http.StatusBadRequest)
		return
	}
	if req.Reason == "" {
		restkit.Error(c, `"reason" is required`, http.StatusBadRequest)
		return
	}

	gr, err := h.Grants.RequestGrant(c.Request.Context(), tenantID, sessionID, granteeID, req.Reason, time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		restkit.ServerError(c, "request grant", err)
		return
	}
	c.JSON(http.StatusCreated, grantView{
		GrantID: gr.GrantID.String(), SessionID: gr.SessionID.String(), GranteeID: gr.GranteeID.String(),
		Reason: gr.Reason, ExpiresAt: gr.ExpiresAt,
	})
}

func (h Handler) handleReadUnderGrant(c *gin.Context) {
	tenantID, userID, ok := restkit.Principal(c)
	if !ok {
		return
	}
	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		restkit.Error(c, "invalid session id", http.StatusBadRequest)
		return
	}

	// The calling principal IS the grantee here — unlike
	// handleRequestContentAccessGrant, where an admin names a DIFFERENT
	// principal to authorize, a read is always "show ME what I was
	// granted," never "show me what someone else was granted."
	dtos, err := h.Grants.Read(c.Request.Context(), tenantID, sessionID, userID)
	if errors.Is(err, contentaccess.ErrNoGrant) {
		restkit.Error(c, "no valid content access grant", http.StatusForbidden)
		return
	}
	if err != nil {
		restkit.ServerError(c, "read under grant", err)
		return
	}
	c.JSON(http.StatusOK, dtos)
}
