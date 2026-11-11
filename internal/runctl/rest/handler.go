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
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
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
	r.POST("/v1/runs/:id/cancel", h.handleCancelRun)
	r.POST("/v1/runs/:id/steer", h.handleSteerRun)
	r.POST("/v1/runs/:id/autonomy", h.handleTightenAutonomy)
	r.POST("/v1/runs/:id/fork", h.handleForkRun)
}

// fail answers an error from run control. The session being absent, in a state
// that refuses the operation, or given an argument it cannot act on is the
// caller's to be told, with the status that says which; anything else is an
// infrastructure failure, which is logged and never echoed (restkit.ServerError).
func fail(c *gin.Context, op string, err error) {
	switch {
	case errors.Is(err, runctl.ErrSessionNotFound):
		restkit.Error(c, "run not found", http.StatusNotFound)
	case errors.Is(err, runctl.ErrSessionState):
		restkit.Error(c, err.Error(), http.StatusConflict)
	case errors.Is(err, runctl.ErrInvalidArgument):
		restkit.Error(c, err.Error(), http.StatusBadRequest)
	default:
		restkit.ServerError(c, op, err)
	}
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

func (h Handler) handleCancelRun(c *gin.Context) {
	tenantID, _, ok := restkit.Principal(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		restkit.Error(c, "invalid run id", http.StatusBadRequest)
		return
	}
	var req cancelRequest
	if c.Request.ContentLength != 0 {
		if !restkit.DecodeJSON(c, &req) {
			return
		}
	}
	if err := h.Control.Cancel(c.Request.Context(), tenantID, id, req.Reason); err != nil {
		fail(c, "cancel", err)
		return
	}
	c.JSON(http.StatusOK, map[string]string{"status": "aborted"})
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
func (h Handler) handleSteerRun(c *gin.Context) {
	tenantID, _, ok := restkit.Principal(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		restkit.Error(c, "invalid run id", http.StatusBadRequest)
		return
	}
	var req steerRequest
	if !restkit.DecodeJSON(c, &req) {
		return
	}
	if err := h.Control.Send(c.Request.Context(), tenantID, id, req.Input); err != nil {
		fail(c, "steer", err)
		return
	}
	c.JSON(http.StatusOK, map[string]string{"status": "steered"})
}

type tightenAutonomyRequest struct {
	Target string `json:"target"`
}

func (h Handler) handleTightenAutonomy(c *gin.Context) {
	tenantID, _, ok := restkit.Principal(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		restkit.Error(c, "invalid run id", http.StatusBadRequest)
		return
	}
	var req tightenAutonomyRequest
	if !restkit.DecodeJSON(c, &req) {
		return
	}
	if err := h.Control.TightenAutonomy(c.Request.Context(), tenantID, id, req.Target); err != nil {
		fail(c, "tighten autonomy", err)
		return
	}
	c.JSON(http.StatusOK, map[string]string{"status": "tightened", "autonomy_level": req.Target})
}

type forkRequest struct {
	AtSeq int64  `json:"at_seq"`
	Model string `json:"model,omitempty"`
}

func (h Handler) handleForkRun(c *gin.Context) {
	tenantID, _, ok := restkit.Principal(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		restkit.Error(c, "invalid run id", http.StatusBadRequest)
		return
	}
	var req forkRequest
	if !restkit.DecodeJSON(c, &req) {
		return
	}
	result, err := h.Control.Fork(c.Request.Context(), tenantID, id, req.AtSeq, runctl.ForkOverrides{ModelID: req.Model})
	if err != nil {
		fail(c, "fork", err)
		return
	}
	c.JSON(http.StatusOK, ForkView{
		SessionID: result.SessionID.String(), DigestDiverged: result.DigestDiverged,
		ParentDigest: hex.EncodeToString(result.ParentDigest), ChildDigest: hex.EncodeToString(result.ChildDigest),
	})
}
