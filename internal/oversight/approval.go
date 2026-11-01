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

// decide runs one human decision on a pending approval, in one unit of work
// with the event that records it: fn applies the decision and appends its
// event to the locked, still-pending ap.
//
// An approval past its TTL that nothing has swept yet is expired here, lazily
// (task 5.9's "on_expiry" is input-specific; an approval's own expiry is
// always a denial-shaped terminal status): the status and its approval_expired
// event are committed, and only then is the decision refused. Refusing inside
// the unit would roll the expiry back with it, leaving the approval pending in
// the table and in ListPending for ever.
func (a *Approvals) decide(ctx context.Context, tenantID, approvalID uuid.UUID, fn func(ctx context.Context, tx Tx, ap *Approval) error) (Approval, error) {
	var ap Approval
	var lapsed bool
	err := a.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		ap, err = tx.LockApproval(ctx, approvalID)
		if err != nil {
			return err
		}
		if ap.Status == ApprovalPending && time.Now().After(ap.ExpiresAt) {
			lapsed = true
			return expireApproval(ctx, tx, tenantID, &ap)
		}
		if ap.Status != ApprovalPending {
			return ErrApprovalNotPending{ApprovalID: approvalID, Status: ap.Status}
		}
		return fn(ctx, tx, &ap)
	})
	if err != nil {
		return Approval{}, err
	}
	if lapsed {
		return Approval{}, ErrApprovalNotPending{ApprovalID: approvalID, Status: ApprovalExpired}
	}
	return ap, nil
}

// expireApproval marks ap expired and records approval_expired, so the status
// never changes without the event that justifies it.
func expireApproval(ctx context.Context, tx Tx, tenantID uuid.UUID, ap *Approval) error {
	if err := tx.SetApprovalStatus(ctx, ap.ApprovalID, ApprovalExpired, "", ""); err != nil {
		return err
	}
	ap.Status = ApprovalExpired
	toolID := ap.ToolID
	_, err := tx.Append(ctx, tenantID, ap.SessionID, eventlog.EventApprovalExpired, &toolID, nil, approvalDecisionPayload{ApprovalID: ap.ApprovalID, ToolID: ap.ToolID})
	return err
}

// Grant approves the tool_use exactly as originally requested — the
// approver's decision executes the tool_use's ORIGINAL input, unmodified.
func (a *Approvals) Grant(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string) (Approval, error) {
	return a.decide(ctx, tenantID, approvalID, func(ctx context.Context, tx Tx, ap *Approval) error {
		if err := tx.SetApprovalStatus(ctx, approvalID, ApprovalGranted, decidedBy, ""); err != nil {
			return err
		}
		ap.Status = ApprovalGranted
		toolID := ap.ToolID
		_, err := tx.Append(ctx, tenantID, ap.SessionID, eventlog.EventApprovalGranted, &toolID, nil, approvalDecisionPayload{ApprovalID: approvalID, ToolID: ap.ToolID, DecidedBy: decidedBy})
		return err
	})
}

// GrantModified approves the tool_use with the approver's SUBSTITUTED
// input — the demo's "modify the recipient at grant time" case (README §5).
// This REBINDS CanonicalDigest to the new input: the whole point of
// "modified" is an intentional divergence from what was originally asked
// about, so re-verifying against the OLD digest at resume time would be
// nonsensical, not a security property — ExecuteApproved (internal/tools/
// pipeline.go) instead re-verifies against this NEW, rebound digest.
func (a *Approvals) GrantModified(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string, modifiedInput json.RawMessage) (Approval, error) {
	return a.decide(ctx, tenantID, approvalID, func(ctx context.Context, tx Tx, ap *Approval) error {
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
}

// Deny refuses the tool_use — the same severity class as a chain-level DENY
// once Kernel.Resume acts on it (kernel/loop.go's Resume doc comment).
func (a *Approvals) Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) (Approval, error) {
	return a.decide(ctx, tenantID, approvalID, func(ctx context.Context, tx Tx, ap *Approval) error {
		if err := tx.SetApprovalStatus(ctx, approvalID, ApprovalDenied, decidedBy, reason); err != nil {
			return err
		}
		ap.Status, ap.Reason = ApprovalDenied, reason
		toolID := ap.ToolID
		_, err := tx.Append(ctx, tenantID, ap.SessionID, eventlog.EventApprovalDenied, &toolID, nil, approvalDecisionPayload{ApprovalID: approvalID, ToolID: ap.ToolID, DecidedBy: decidedBy, Reason: reason})
		return err
	})
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
