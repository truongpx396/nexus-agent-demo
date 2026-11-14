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

	"github.com/truongpx396/nexus-agent/internal/contentaccess"
	restkit "github.com/truongpx396/nexus-agent/internal/surfaces/rest"
)

// Handler serves the grant endpoints. Grants is mandatory.
type Handler struct {
	Grants *contentaccess.Grants
}

var (
	_ restkit.Module  = Handler{}
	_ ServerInterface = Handler{}
)

// The routes, the request and response models and ServerInterface are generated
// from the operations tagged "content-access" in api/openapi.yaml (api.gen.go).
//
//go:generate go tool oapi-codegen -config oapi-codegen.yaml ../../../api/openapi.yaml

// Routes implements restkit.Module.
func (h Handler) Routes(r restkit.Router) {
	RegisterHandlersWithOptions(r.Authenticated(), h, GinServerOptions{ErrorHandler: restkit.ParamError})
}

// Content access grants (README task 5.11) — the only path to plaintext
// outside a run's own audience. Distinct from StreamRunEvents (internal/runs/
// rest), which is the run's OWN owning user reading their own session's content
// (pattern #51, gated on sess.UserID == userID): here the requester and the
// grantee are independent principals, and every grant AND every read produces
// its own audited receipt (internal/contentaccess.Grants).

// RequestContentAccessGrant is POST /v1/sessions/{id}/content-access-grants.
func (h Handler) RequestContentAccessGrant(c *gin.Context, sessionID SessionID) {
	tenantID, _, ok := restkit.Principal(c)
	if !ok {
		return
	}
	var req RequestGrantRequest
	if !restkit.DecodeJSON(c, &req) {
		return
	}
	// A grantee_id that is present but not a uuid already failed to decode (400);
	// an absent one decodes to the zero uuid.
	granteeID := req.GranteeID
	if granteeID == uuid.Nil {
		restkit.Error(c, `"grantee_id" is required`, http.StatusBadRequest)
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
	c.JSON(http.StatusCreated, Grant{
		GrantID: gr.GrantID, SessionID: gr.SessionID, GranteeID: gr.GranteeID,
		Reason: gr.Reason, ExpiresAt: gr.ExpiresAt,
	})
}

// ReadUnderContentAccessGrant is GET /v1/sessions/{id}/content-access-grants/read.
func (h Handler) ReadUnderContentAccessGrant(c *gin.Context, sessionID SessionID) {
	tenantID, userID, ok := restkit.Principal(c)
	if !ok {
		return
	}

	// The calling principal IS the grantee here — unlike
	// RequestContentAccessGrant, where an admin names a DIFFERENT principal to
	// authorize, a read is always "show ME what I was granted," never "show me
	// what someone else was granted."
	dtos, err := h.Grants.Read(c.Request.Context(), tenantID, sessionID, userID)
	if errors.Is(err, contentaccess.ErrNoGrant) {
		restkit.Error(c, "no valid content access grant", http.StatusForbidden)
		return
	}
	if err != nil {
		restkit.ServerError(c, "read under grant", err)
		return
	}
	events := make([]GrantedEvent, len(dtos))
	for i, e := range dtos {
		events[i] = toGrantedEvent(e)
	}
	c.JSON(http.StatusOK, events)
}

// toGrantedEvent copies one decrypted event into the wire shape, whose keys are
// the capitalised field names contentaccess.EventDTO has always been encoded
// with (api/openapi.yaml's GrantedEvent says so).
func toGrantedEvent(e contentaccess.EventDTO) GrantedEvent {
	ev := GrantedEvent{EventID: e.EventID, Seq: e.Seq, Type: string(e.Type), CreatedAt: e.CreatedAt}
	if e.Body != nil {
		ev.Body = &e.Body
	}
	return ev
}
