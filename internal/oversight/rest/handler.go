// Package rest is the REST binding of human oversight: list and read the
// approvals waiting on a person, and grant, modify or deny one (README tasks
// 5.6-5.8). A thin translator over internal/oversight: it decodes the request,
// calls the use case, and renders the result. Who may decide, whether the
// approval is still pending, and resuming the suspended run are oversight's.
//
// The routes mount through the kit in internal/surfaces/rest.
package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

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
	Grant(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string) oversight.Outcome
	GrantModified(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string, modifiedInput json.RawMessage) oversight.Outcome
	Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) oversight.Outcome
}

// Handler serves the approval endpoints. Both fields are mandatory.
type Handler struct {
	Approvals ApprovalSource
	Decisions DecisionSource
}

var _ restkit.Module = Handler{}

// Routes implements restkit.Module.
func (h Handler) Routes(r restkit.Router) {
	r.Handle("GET /v1/approvals", h.handleListApprovals)
	r.Handle("GET /v1/approvals/{id}", h.handleGetApproval)
	r.Handle("POST /v1/approvals/{id}/grant", h.handleGrantApproval)
	r.Handle("POST /v1/approvals/{id}/deny", h.handleDenyApproval)
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

// ResumeOutcome summarizes what granting/denying an approval actually did —
// the HTTP response for all three decisions below. It deliberately doesn't
// repeat the run's status/terminal_reason: GET /v1/runs/{id} (already the one
// place that answers "is this run done yet") is the source for that, and a
// caller that wants the full live event stream can subscribe to GET
// /v1/runs/{id}/events — this response is only a synchronous "the resume
// happened, here's the session and how many events it produced"
// acknowledgment.
type ResumeOutcome struct {
	SessionID      string `json:"session_id"`
	EventsAppended int    `json:"events_appended"`
	Err            string `json:"error,omitempty"`
}

func toApprovalView(ap oversight.Approval) ApprovalView {
	ctxJSON, _ := json.Marshal(ap.Context) // ContextPackage is always marshalable (json.RawMessage + strings)
	return ApprovalView{
		ApprovalID: ap.ApprovalID.String(), SessionID: ap.SessionID.String(), ToolID: ap.ToolID,
		AskKind: ap.AskKind, Status: string(ap.Status), Context: ctxJSON, ExpiresAt: ap.ExpiresAt, CreatedAt: ap.CreatedAt,
	}
}

func toResumeOutcome(o oversight.Outcome) ResumeOutcome {
	return ResumeOutcome{SessionID: o.SessionID, EventsAppended: o.EventsAppended, Err: o.Err}
}

func (h Handler) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := restkit.Principal(w, r)
	if !ok {
		return
	}
	approvals, err := h.Approvals.ListPending(r.Context(), tenantID)
	if err != nil {
		restkit.ServerError(w, r, "list approvals", err)
		return
	}
	views := make([]ApprovalView, len(approvals))
	for i, ap := range approvals {
		views[i] = toApprovalView(ap)
	}
	restkit.WriteJSON(w, http.StatusOK, views)
}

func (h Handler) handleGetApproval(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := restkit.Principal(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid approval id", http.StatusBadRequest)
		return
	}
	ap, err := h.Approvals.Get(r.Context(), tenantID, id)
	if errors.Is(err, oversight.ErrNotFound) {
		http.Error(w, "approval not found", http.StatusNotFound)
		return
	}
	if err != nil {
		restkit.ServerError(w, r, "get approval", err)
		return
	}
	restkit.WriteJSON(w, http.StatusOK, toApprovalView(ap))
}

type grantApprovalRequest struct {
	// ModifiedInput, if set, is the approver's substitution — README §5's
	// "modify the recipient at grant time" demo case
	// (internal/oversight.Approvals.GrantModified).
	ModifiedInput json.RawMessage `json:"modified_input,omitempty"`
}

func (h Handler) handleGrantApproval(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := restkit.Principal(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid approval id", http.StatusBadRequest)
		return
	}
	var req grantApprovalRequest
	if r.ContentLength != 0 {
		if !restkit.DecodeJSON(w, r, &req) {
			return
		}
	}

	var outcome oversight.Outcome
	if len(req.ModifiedInput) > 0 {
		outcome = h.Decisions.GrantModified(r.Context(), tenantID, id, userID.String(), req.ModifiedInput)
	} else {
		outcome = h.Decisions.Grant(r.Context(), tenantID, id, userID.String())
	}
	restkit.WriteJSON(w, http.StatusOK, toResumeOutcome(outcome))
}

type denyApprovalRequest struct {
	Reason string `json:"reason"`
}

func (h Handler) handleDenyApproval(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := restkit.Principal(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid approval id", http.StatusBadRequest)
		return
	}
	var req denyApprovalRequest
	if r.ContentLength != 0 {
		if !restkit.DecodeJSON(w, r, &req) {
			return
		}
	}

	restkit.WriteJSON(w, http.StatusOK, toResumeOutcome(h.Decisions.Deny(r.Context(), tenantID, id, userID.String(), req.Reason)))
}
