package oversight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// ErrNotFound is returned (wrapped) when an approval or input request
// doesn't exist in the caller's tenant scope — either it never existed, or
// RLS made a cross-tenant row invisible, which are indistinguishable on
// purpose (internal/store's own convention throughout this codebase).
var ErrNotFound = errors.New("oversight: not found")

// deps is what Approvals and Inputs share: the unit-of-work Store every
// decision runs in.
type deps struct {
	Store Store
}

// Approvals is the full approval transaction (README task 5.6): digest
// binding, a decision-ready context package, a named assignee, a TTL, and
// the five terminal-ish statuses (granted/granted_modified/denied/expired/
// invalidated). Construct once, share across the process.
type Approvals struct {
	deps
	// DefaultTTL overrides DefaultApprovalTTL when set.
	DefaultTTL time.Duration
}

// NewApprovals returns Approvals over store. Every decision runs in one unit
// of work with the event that records it (Tx embeds eventlog.Appender), so a
// status never changes without its event and an event never exists for a
// status that did not change.
func NewApprovals(store Store) *Approvals {
	return &Approvals{deps: deps{Store: store}}
}

func (a *Approvals) ttl() time.Duration {
	if a.DefaultTTL > 0 {
		return a.DefaultTTL
	}
	return DefaultApprovalTTL
}

// CreateApprovalRequest is everything Create needs. Assignee, if empty,
// defaults to the session's owning user_id — there is no RBAC/approver
// directory yet (Phase 7's internal/config), so "whoever owns the run" is
// the only honest default.
type CreateApprovalRequest struct {
	TenantID        uuid.UUID
	SessionID       uuid.UUID
	ToolUseEventID  uuid.UUID
	ToolID          string
	AskKind         string
	CanonicalDigest []byte
	Context         ContextPackage
	Assignee        string
}

// Create durably records an approval bound to req.CanonicalDigest — the
// EventApprovalRequested this gates was already appended by kernel/loop.go's
// suspendForApproval before this runs (kernel.OnSuspend, wired from
// cmd/nexusd); Create's own job is only the durable, resolvable record a
// human decision acts on.
func (a *Approvals) Create(ctx context.Context, req CreateApprovalRequest) (Approval, error) {
	ap := Approval{
		ApprovalID: uuid.New(), TenantID: req.TenantID, SessionID: req.SessionID,
		ToolUseEventID: req.ToolUseEventID, ToolID: req.ToolID, AskKind: req.AskKind,
		CanonicalDigest: req.CanonicalDigest, Context: req.Context, Assignee: req.Assignee,
		Status: ApprovalPending, ExpiresAt: time.Now().Add(a.ttl()),
	}

	err := a.Store.InTenant(ctx, req.TenantID, func(ctx context.Context, tx Tx) error {
		if ap.Assignee == "" {
			owner, err := tx.SessionOwner(ctx, req.SessionID)
			if err != nil {
				return err
			}
			ap.Assignee = owner.String()
		}
		return tx.InsertApproval(ctx, &ap)
	})
	return ap, err
}

// Get loads one approval by id.
func (a *Approvals) Get(ctx context.Context, tenantID, approvalID uuid.UUID) (Approval, error) {
	var ap Approval
	err := a.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		ap, err = tx.GetApproval(ctx, approvalID)
		return err
	})
	return ap, err
}

// ErrApprovalNotPending is returned by Grant/GrantModified/Deny when the
// approval has already left the pending state (already decided, already
// expired, already invalidated) — a second decision on the same approval
// must never silently overwrite the first.
type ErrApprovalNotPending struct {
	ApprovalID uuid.UUID
	Status     ApprovalStatus
}

func (e ErrApprovalNotPending) Error() string {
	return fmt.Sprintf("oversight: approval %s is %s, not pending", e.ApprovalID, e.Status)
}

// checkPending loads ap (locking it) and fails closed unless it is still
// pending and unexpired — lazily expiring it (task 5.9's "on_expiry" is
// input-specific; an approval's own expiry is always a denial-shaped terminal
// status) if its TTL has passed but nothing swept it yet.
func (a *Approvals) checkPending(ctx context.Context, tx Tx, approvalID uuid.UUID) (Approval, error) {
	ap, err := tx.LockApproval(ctx, approvalID)
	if err != nil {
		return Approval{}, err
	}
	if ap.Status == ApprovalPending && time.Now().After(ap.ExpiresAt) {
		if err := tx.SetApprovalStatus(ctx, ap.ApprovalID, ApprovalExpired, "", ""); err != nil {
			return Approval{}, err
		}
		ap.Status = ApprovalExpired
	}
	if ap.Status != ApprovalPending {
		return Approval{}, ErrApprovalNotPending{ApprovalID: approvalID, Status: ap.Status}
	}
	return ap, nil
}

// Grant approves the tool_use exactly as originally requested — the
// approver's decision executes the tool_use's ORIGINAL input, unmodified.
func (a *Approvals) Grant(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string) (Approval, error) {
	var ap Approval
	err := a.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		ap, err = a.checkPending(ctx, tx, approvalID)
		if err != nil {
			return err
		}
		if err := tx.SetApprovalStatus(ctx, approvalID, ApprovalGranted, decidedBy, ""); err != nil {
			return err
		}
		ap.Status = ApprovalGranted
		toolID := ap.ToolID
		_, err = tx.Append(ctx, tenantID, ap.SessionID, eventlog.EventApprovalGranted, &toolID, nil, approvalDecisionPayload{ApprovalID: approvalID, ToolID: ap.ToolID, DecidedBy: decidedBy})
		return err
	})
	return ap, err
}

// GrantModified approves the tool_use with the approver's SUBSTITUTED
// input — the demo's "modify the recipient at grant time" case (README §5).
// This REBINDS CanonicalDigest to the new input: the whole point of
// "modified" is an intentional divergence from what was originally asked
// about, so re-verifying against the OLD digest at resume time would be
// nonsensical, not a security property — ExecuteApproved (internal/tools/
// pipeline.go) instead re-verifies against this NEW, rebound digest.
func (a *Approvals) GrantModified(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string, modifiedInput json.RawMessage) (Approval, error) {
	var ap Approval
	err := a.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		ap, err = a.checkPending(ctx, tx, approvalID)
		if err != nil {
			return err
		}
		newDigest, err := tools.CanonicalDigest(ap.ToolID, modifiedInput)
		if err != nil {
			return fmt.Errorf("oversight: canonical digest for modified input: %w", err)
		}
		if err := tx.SetApprovalModified(ctx, approvalID, decidedBy, modifiedInput, newDigest); err != nil {
			return err
		}
		ap.Status, ap.GrantedInput, ap.CanonicalDigest = ApprovalGrantedModified, modifiedInput, newDigest
		toolID := ap.ToolID
		_, err = tx.Append(ctx, tenantID, ap.SessionID, eventlog.EventApprovalGrantedModified, &toolID, nil, approvalDecisionPayload{ApprovalID: approvalID, ToolID: ap.ToolID, DecidedBy: decidedBy, Input: modifiedInput})
		return err
	})
	return ap, err
}

// Deny refuses the tool_use — the same severity class as a chain-level DENY
// once Kernel.Resume acts on it (kernel/loop.go's Resume doc comment).
func (a *Approvals) Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) (Approval, error) {
	var ap Approval
	err := a.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		ap, err = a.checkPending(ctx, tx, approvalID)
		if err != nil {
			return err
		}
		if err := tx.SetApprovalStatus(ctx, approvalID, ApprovalDenied, decidedBy, reason); err != nil {
			return err
		}
		ap.Status, ap.Reason = ApprovalDenied, reason
		toolID := ap.ToolID
		_, err = tx.Append(ctx, tenantID, ap.SessionID, eventlog.EventApprovalDenied, &toolID, nil, approvalDecisionPayload{ApprovalID: approvalID, ToolID: ap.ToolID, DecidedBy: decidedBy, Reason: reason})
		return err
	})
	return ap, err
}

// ListPending returns every pending approval for tenantID — nexusctl
// approvals show / the REST list endpoint's source.
func (a *Approvals) ListPending(ctx context.Context, tenantID uuid.UUID) ([]Approval, error) {
	var out []Approval
	err := a.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		out, err = tx.PendingApprovals(ctx)
		return err
	})
	return out, err
}

type approvalDecisionPayload struct {
	ApprovalID uuid.UUID       `json:"approval_id"`
	ToolID     string          `json:"tool_id"`
	DecidedBy  string          `json:"decided_by,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
}
