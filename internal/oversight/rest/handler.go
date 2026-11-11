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

var (
	_ restkit.Module  = Handler{}
	_ ServerInterface = Handler{}
)

// The routes, the request and response models and ServerInterface are generated
// from the operations tagged "approvals" in api/openapi.yaml (api.gen.go).
//
//go:generate go tool oapi-codegen -config oapi-codegen.yaml ../../../api/openapi.yaml

// Routes implements restkit.Module.
func (h Handler) Routes(r restkit.Router) {
	RegisterHandlersWithOptions(r.Authenticated(), h, GinServerOptions{ErrorHandler: restkit.ParamError})
}

// toDecisionResponse says what became of the run an approval suspended. The
// decision is durable when it is sent; the run continues asynchronously, so
// there is no count of events to report. "resuming": the run is queued to
// continue and its events arrive on GET /v1/runs/{id}/events like any other
// run's. "decided": the hand-off to the queue failed; recovery resumes the
// run and nothing needs to be re-sent.
func toDecisionResponse(o oversight.Outcome) DecisionResponse {
	status := DecisionResponseStatusDecided
	if o.Resuming {
		status = DecisionResponseStatusResuming
	}
	return DecisionResponse{ApprovalID: o.ApprovalID, SessionID: o.SessionID, Status: status}
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

// toApproval renders one approval. Context is the decision-ready rendering
// (README §5's Phase 5 demo: "renders recipient/subject/attachment digests ...
// never a bare UUID") — passed through as-is; this package has no reason to
// know its shape.
func toApproval(ap oversight.Approval) Approval {
	ctxJSON, _ := json.Marshal(ap.Context) // ContextPackage is always marshalable (json.RawMessage + strings)
	return Approval{
		ApprovalID: ap.ApprovalID, SessionID: ap.SessionID, ToolID: ap.ToolID,
		AskKind: ap.AskKind, Status: ApprovalStatus(ap.Status), Context: ctxJSON, ExpiresAt: ap.ExpiresAt, CreatedAt: ap.CreatedAt,
	}
}

// ListApprovals is GET /v1/approvals.
func (h Handler) ListApprovals(c *gin.Context) {
	tenantID, _, ok := restkit.Principal(c)
	if !ok {
		return
	}
	approvals, err := h.Approvals.ListPending(c.Request.Context(), tenantID)
	if err != nil {
		restkit.ServerError(c, "list approvals", err)
		return
	}
	views := make([]Approval, len(approvals))
	for i, ap := range approvals {
		views[i] = toApproval(ap)
	}
	c.JSON(http.StatusOK, views)
}

// GetApproval is GET /v1/approvals/{id}.
func (h Handler) GetApproval(c *gin.Context, id ApprovalID) {
	tenantID, _, ok := restkit.Principal(c)
	if !ok {
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
	c.JSON(http.StatusOK, toApproval(ap))
}

// GrantApproval is POST /v1/approvals/{id}/grant. ModifiedInput, if set, is the
// approver's substitution — README §5's "modify the recipient at grant time"
// demo case (internal/oversight.Approvals.GrantModified).
func (h Handler) GrantApproval(c *gin.Context, id ApprovalID) {
	tenantID, userID, ok := restkit.Principal(c)
	if !ok {
		return
	}
	var req GrantApprovalRequest
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

// DenyApproval is POST /v1/approvals/{id}/deny.
func (h Handler) DenyApproval(c *gin.Context, id ApprovalID) {
	tenantID, userID, ok := restkit.Principal(c)
	if !ok {
		return
	}
	var req DenyApprovalRequest
	if c.Request.ContentLength != 0 {
		if !restkit.DecodeJSON(c, &req) {
			return
		}
	}

	outcome, derr := h.Decisions.Deny(c.Request.Context(), tenantID, id, userID.String(), req.Reason)
	respondDecision(c, outcome, derr)
}
