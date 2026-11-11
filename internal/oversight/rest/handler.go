// Package rest is the REST binding of human oversight: list and read the
// approvals waiting on a person, and grant, modify or deny one (README tasks
// 5.6-5.8). A thin translator over internal/oversight: it decodes the request,
// calls the use case, and renders the result. Who may decide and whether the
// approval is still pending are oversight's, and so is resuming the suspended
// run, which a decision only schedules.
//
// The routes mount through the kit in internal/surfaces/rest.
package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	restkit "github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
)

// ApprovalSource is the part of oversight.Approvals the endpoints read through.
type ApprovalSource interface {
	Get(ctx context.Context, tenantID, approvalID uuid.UUID) (oversight.Approval, error)
	ListPending(ctx context.Context, tenantID uuid.UUID) ([]oversight.Approval, error)
}

// DecisionSource is the part of oversight.Decisions the endpoints decide
// through.
type DecisionSource interface {
	Grant(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string) (oversight.Outcome, error)
	GrantModified(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string, modifiedInput json.RawMessage) (oversight.Outcome, error)
	Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) (oversight.Outcome, error)
}

// Handler serves the approval endpoints. Both fields are mandatory.
type Handler struct {
	Approvals ApprovalSource
	Decisions DecisionSource
}

var _ restkit.Module = Handler{}

// Routes implements restkit.Module.
func (h Handler) Routes(r restkit.Router) {
	r.GET("/v1/approvals", h.handleListApprovals)
	r.GET("/v1/approvals/:id", h.handleGetApproval)
	r.POST("/v1/approvals/:id/grant", h.handleGrantApproval)
	r.POST("/v1/approvals/:id/deny", h.handleDenyApproval)
}

// ApprovalView is what this surface exposes for one approval. Context is the
// decision-ready rendering (README §5's Phase 5 demo: "renders recipient/
// subject/attachment digests ... never a bare UUID") — passed through as-is;
// this package has no reason to know its shape.
type ApprovalView struct {
	ApprovalID string          `json:"approval_id"`
	SessionID  string          `json:"session_id"`
	ToolID     string          `json:"tool_id"`
	AskKind    string          `json:"ask_kind"`
	Status     string          `json:"status"`
	Context    json.RawMessage `json:"context"`
	ExpiresAt  time.Time       `json:"expires_at"`
	CreatedAt  time.Time       `json:"created_at"`
}

// DecisionStatus says what became of the run an approval suspended.
const (
	// StatusResuming: the decision is recorded and the run is queued to continue.
	// Its events arrive on GET /v1/runs/{id}/events like any other run's.
	StatusResuming = "resuming"
	// StatusDecided: the decision is recorded but the hand-off to the queue
	// failed. Recovery resumes the run; nothing needs to be re-sent.
	StatusDecided = "decided"
)

// DecisionResponse is the answer to a grant or a deny. The decision is durable
// when it is sent; the run continues asynchronously, so there is no count of
// events to report.
type DecisionResponse struct {
	ApprovalID string `json:"approval_id"`
	SessionID  string `json:"session_id"`
	Status     string `json:"status"`
}

func toDecisionResponse(o oversight.Outcome) DecisionResponse {
	status := StatusDecided
	if o.Resuming {
		status = StatusResuming
	}
	return DecisionResponse{ApprovalID: o.ApprovalID.String(), SessionID: o.SessionID.String(), Status: status}
}

// respondDecision answers a decision: 202 once it is recorded, 404 for an
// approval that is not there (or not this tenant's), 409 for one that is no longer
// pending, and an opaque 500 for anything else.
func respondDecision(c *gin.Context, out oversight.Outcome, err error) {
	if notPending, ok := errors.AsType[oversight.ApprovalNotPendingError](err); ok {
		restkit.Error(c, notPending.Error(), http.StatusConflict)
		return
	}
	switch {
	case errors.Is(err, oversight.ErrNotFound):
		restkit.Error(c, "approval not found", http.StatusNotFound)
	case err != nil:
		restkit.ServerError(c, "decide approval", err)
	default:
		c.JSON(http.StatusAccepted, toDecisionResponse(out))
	}
}

func toApprovalView(ap oversight.Approval) ApprovalView {
	ctxJSON, _ := json.Marshal(ap.Context) // ContextPackage is always marshalable (json.RawMessage + strings)
	return ApprovalView{
		ApprovalID: ap.ApprovalID.String(), SessionID: ap.SessionID.String(), ToolID: ap.ToolID,
		AskKind: ap.AskKind, Status: string(ap.Status), Context: ctxJSON, ExpiresAt: ap.ExpiresAt, CreatedAt: ap.CreatedAt,
	}
}

func (h Handler) handleListApprovals(c *gin.Context) {
	tenantID, _, ok := restkit.Principal(c)
	if !ok {
		return
	}
	approvals, err := h.Approvals.ListPending(c.Request.Context(), tenantID)
	if err != nil {
		restkit.ServerError(c, "list approvals", err)
		return
	}
	views := make([]ApprovalView, len(approvals))
	for i, ap := range approvals {
		views[i] = toApprovalView(ap)
	}
	c.JSON(http.StatusOK, views)
}

func (h Handler) handleGetApproval(c *gin.Context) {
	tenantID, _, ok := restkit.Principal(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		restkit.Error(c, "invalid approval id", http.StatusBadRequest)
		return
	}
	ap, err := h.Approvals.Get(c.Request.Context(), tenantID, id)
	if errors.Is(err, oversight.ErrNotFound) {
		restkit.Error(c, "approval not found", http.StatusNotFound)
		return
	}
	if err != nil {
		restkit.ServerError(c, "get approval", err)
		return
	}
	c.JSON(http.StatusOK, toApprovalView(ap))
}

type grantApprovalRequest struct {
	// ModifiedInput, if set, is the approver's substitution — README §5's
	// "modify the recipient at grant time" demo case
	// (internal/oversight.Approvals.GrantModified).
	ModifiedInput json.RawMessage `json:"modified_input,omitempty"`
}

func (h Handler) handleGrantApproval(c *gin.Context) {
	tenantID, userID, ok := restkit.Principal(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		restkit.Error(c, "invalid approval id", http.StatusBadRequest)
		return
	}
	var req grantApprovalRequest
	if c.Request.ContentLength != 0 {
		if !restkit.DecodeJSON(c, &req) {
			return
		}
	}

	var (
		outcome oversight.Outcome
		derr    error
	)
	if len(req.ModifiedInput) > 0 {
		outcome, derr = h.Decisions.GrantModified(c.Request.Context(), tenantID, id, userID.String(), req.ModifiedInput)
	} else {
		outcome, derr = h.Decisions.Grant(c.Request.Context(), tenantID, id, userID.String())
	}
	respondDecision(c, outcome, derr)
}

type denyApprovalRequest struct {
	Reason string `json:"reason"`
}

func (h Handler) handleDenyApproval(c *gin.Context) {
	tenantID, userID, ok := restkit.Principal(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		restkit.Error(c, "invalid approval id", http.StatusBadRequest)
		return
	}
	var req denyApprovalRequest
	if c.Request.ContentLength != 0 {
		if !restkit.DecodeJSON(c, &req) {
			return
		}
	}

	outcome, derr := h.Decisions.Deny(c.Request.Context(), tenantID, id, userID.String(), req.Reason)
	respondDecision(c, outcome, derr)
}
