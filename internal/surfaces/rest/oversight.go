package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// ApprovalView is what this surface exposes for one approval — translated
// from internal/oversight.Approval by whatever OversightPort implementation
// cmd/nexusd wires in. This package must not import internal/oversight
// directly: it transitively imports kernel (for Kernel.Resume), and
// tests/contract/boundaries_test.go's "surfaces must not import the kernel"
// rule walks the FULL transitive import graph, not just direct imports —
// exactly the reason RunStarter/SealFunc already exist as a seam for
// kernel.Kernel.Run one file up (server.go/starter.go). Context is the
// decision-ready rendering (README §5's Phase 5 demo: "renders recipient/
// subject/attachment digests ... never a bare UUID") — passed through
// as-is; this package has no reason to know its shape.
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
// the HTTP response for all three oversight actions below. It deliberately
// doesn't repeat the run's status/terminal_reason: GET /v1/runs/{id}
// (already the one place that answers "is this run done yet") is the
// source for that, and a caller that wants the full live event stream can
// subscribe to GET /v1/runs/{id}/events — this response is only a
// synchronous "the resume happened, here's the session and how many events
// it produced" acknowledgment.
type ResumeOutcome struct {
	SessionID      string `json:"session_id"`
	EventsAppended int    `json:"events_appended"`
	Err            string `json:"error,omitempty"`
}

// OversightPort is the entire seam between this surface and
// internal/oversight — cmd/nexusd supplies the concrete implementation,
// backed by oversight.Approvals + oversight.Resumer, the only place in the
// binary allowed to import both this package and internal/oversight (the
// same exemption starter.go's own doc comment already grants
// kernelRunStarter for kernel.Kernel.Run).
type OversightPort interface {
	GetApproval(ctx context.Context, tenantID, approvalID uuid.UUID) (ApprovalView, error)
	ListPendingApprovals(ctx context.Context, tenantID uuid.UUID) ([]ApprovalView, error)
	Grant(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string) ResumeOutcome
	GrantModified(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string, modifiedInput json.RawMessage) ResumeOutcome
	Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) ResumeOutcome
}

func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := s.principal(w, r)
	if !ok {
		return
	}
	views, err := s.Oversight.ListPendingApprovals(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "list approvals: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleGetApproval(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := s.principal(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid approval id", http.StatusBadRequest)
		return
	}
	view, err := s.Oversight.GetApproval(r.Context(), tenantID, id)
	if err != nil {
		http.Error(w, "approval not found: "+err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

type grantApprovalRequest struct {
	// ModifiedInput, if set, is the approver's substitution — README §5's
	// "modify the recipient at grant time" demo case
	// (internal/oversight.Approvals.GrantModified).
	ModifiedInput json.RawMessage `json:"modified_input,omitempty"`
}

func (s *Server) handleGrantApproval(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := s.principal(w, r)
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}

	var outcome ResumeOutcome
	if len(req.ModifiedInput) > 0 {
		outcome = s.Oversight.GrantModified(r.Context(), tenantID, id, userID.String(), req.ModifiedInput)
	} else {
		outcome = s.Oversight.Grant(r.Context(), tenantID, id, userID.String())
	}
	writeJSON(w, http.StatusOK, outcome)
}

type denyApprovalRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) handleDenyApproval(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := s.principal(w, r)
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}

	outcome := s.Oversight.Deny(r.Context(), tenantID, id, userID.String(), req.Reason)
	writeJSON(w, http.StatusOK, outcome)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
