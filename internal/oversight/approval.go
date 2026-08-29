package oversight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// ErrNotFound is returned (wrapped) when an approval or input request
// doesn't exist in the caller's tenant scope — either it never existed, or
// RLS made a cross-tenant row invisible, which are indistinguishable on
// purpose (internal/store's own convention throughout this codebase).
var ErrNotFound = errors.New("oversight: not found")

// Approvals is the full approval transaction (README task 5.6): digest
// binding, a decision-ready context package, a named assignee, a TTL, and
// the five terminal-ish statuses (granted/granted_modified/denied/expired/
// invalidated). Construct once, share across the process.
type Approvals struct {
	deps
	// DefaultTTL overrides DefaultApprovalTTL when set.
	DefaultTTL time.Duration
}

func NewApprovals(st *store.Store, keys *crypto.KeyStore, chain *audit.Chain) *Approvals {
	return &Approvals{deps: deps{Store: st, Keys: keys, Chain: chain}}
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

	err := a.Store.InTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if ap.Assignee == "" {
			sess, err := store.GetSession(ctx, tx, req.SessionID)
			if err != nil {
				return err
			}
			ap.Assignee = sess.UserID.String()
		}
		return insertApproval(ctx, tx, &ap)
	})
	return ap, err
}

// Get loads one approval by id.
func (a *Approvals) Get(ctx context.Context, tenantID, approvalID uuid.UUID) (Approval, error) {
	var ap Approval
	err := a.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ap, err = scanApproval(tx.QueryRow(ctx, approvalSelectSQL+` WHERE approval_id = $1`, approvalID))
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

// checkPending loads ap and fails closed unless it is still pending and
// unexpired — lazily expiring it (task 5.9's "on_expiry" is input-specific;
// an approval's own expiry is always a denial-shaped terminal status) if
// its TTL has passed but nothing swept it yet.
func (a *Approvals) checkPending(ctx context.Context, tx pgx.Tx, tenantID, approvalID uuid.UUID) (Approval, error) {
	ap, err := scanApproval(tx.QueryRow(ctx, approvalSelectSQL+` WHERE approval_id = $1 FOR UPDATE`, approvalID))
	if err != nil {
		return Approval{}, err
	}
	if ap.Status == ApprovalPending && time.Now().After(ap.ExpiresAt) {
		if err := updateApprovalStatus(ctx, tx, ap.ApprovalID, ApprovalExpired, "", nil, ""); err != nil {
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
	err := a.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ap, err = a.checkPending(ctx, tx, tenantID, approvalID)
		if err != nil {
			return err
		}
		if err := updateApprovalStatus(ctx, tx, approvalID, ApprovalGranted, decidedBy, nil, ""); err != nil {
			return err
		}
		ap.Status = ApprovalGranted
		toolID := ap.ToolID
		_, err = a.appendEvent(ctx, tx, tenantID, ap.SessionID, store.EventApprovalGranted, &toolID, nil, approvalDecisionPayload{ApprovalID: approvalID, ToolID: ap.ToolID, DecidedBy: decidedBy})
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
	err := a.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ap, err = a.checkPending(ctx, tx, tenantID, approvalID)
		if err != nil {
			return err
		}
		newDigest, err := tools.CanonicalDigest(ap.ToolID, modifiedInput)
		if err != nil {
			return fmt.Errorf("oversight: canonical digest for modified input: %w", err)
		}
		if err := updateApprovalModified(ctx, tx, approvalID, decidedBy, modifiedInput, newDigest); err != nil {
			return err
		}
		ap.Status, ap.GrantedInput, ap.CanonicalDigest = ApprovalGrantedModified, modifiedInput, newDigest
		toolID := ap.ToolID
		_, err = a.appendEvent(ctx, tx, tenantID, ap.SessionID, store.EventApprovalGrantedModified, &toolID, nil, approvalDecisionPayload{ApprovalID: approvalID, ToolID: ap.ToolID, DecidedBy: decidedBy, Input: modifiedInput})
		return err
	})
	return ap, err
}

// Deny refuses the tool_use — the same severity class as a chain-level DENY
// once Kernel.Resume acts on it (kernel/loop.go's Resume doc comment).
func (a *Approvals) Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) (Approval, error) {
	var ap Approval
	err := a.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ap, err = a.checkPending(ctx, tx, tenantID, approvalID)
		if err != nil {
			return err
		}
		if err := updateApprovalStatus(ctx, tx, approvalID, ApprovalDenied, decidedBy, nil, reason); err != nil {
			return err
		}
		ap.Status, ap.Reason = ApprovalDenied, reason
		toolID := ap.ToolID
		_, err = a.appendEvent(ctx, tx, tenantID, ap.SessionID, store.EventApprovalDenied, &toolID, nil, approvalDecisionPayload{ApprovalID: approvalID, ToolID: ap.ToolID, DecidedBy: decidedBy, Reason: reason})
		return err
	})
	return ap, err
}

// ListPending returns every pending approval for tenantID — nexusctl
// approvals show / the REST list endpoint's source.
func (a *Approvals) ListPending(ctx context.Context, tenantID uuid.UUID) ([]Approval, error) {
	var out []Approval
	err := a.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, approvalSelectSQL+` WHERE status = 'pending' ORDER BY created_at ASC`)
		if err != nil {
			return fmt.Errorf("oversight: list pending approvals: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			ap, err := scanApprovalRows(rows)
			if err != nil {
				return err
			}
			out = append(out, ap)
		}
		return rows.Err()
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

const approvalSelectSQL = `
	SELECT approval_id, tenant_id, session_id, tool_use_event_id, tool_id, ask_kind,
	       canonical_digest, context_package, assignee, status, granted_input,
	       expires_at, decided_at, decided_by, reason, created_at
	FROM approvals`

// row is the subset of pgx.Row and pgx.Rows scanApproval needs — lets one
// scan function serve both Get (pgx.Row) and ListPending (pgx.Rows).
type row interface {
	Scan(dest ...any) error
}

func scanApproval(r row) (Approval, error) {
	return scanApprovalRows(r)
}

func scanApprovalRows(r row) (Approval, error) {
	var ap Approval
	var contextJSON, grantedInput []byte
	var decidedBy, reason *string
	err := r.Scan(
		&ap.ApprovalID, &ap.TenantID, &ap.SessionID, &ap.ToolUseEventID, &ap.ToolID, &ap.AskKind,
		&ap.CanonicalDigest, &contextJSON, &ap.Assignee, &ap.Status, &grantedInput,
		&ap.ExpiresAt, &ap.DecidedAt, &decidedBy, &reason, &ap.CreatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Approval{}, fmt.Errorf("%w: approval", ErrNotFound)
		}
		return Approval{}, fmt.Errorf("oversight: scan approval: %w", err)
	}
	if len(contextJSON) > 0 {
		if err := json.Unmarshal(contextJSON, &ap.Context); err != nil {
			return Approval{}, fmt.Errorf("oversight: unmarshal context_package: %w", err)
		}
	}
	if len(grantedInput) > 0 {
		ap.GrantedInput = json.RawMessage(grantedInput)
	}
	if decidedBy != nil {
		ap.DecidedBy = *decidedBy
	}
	if reason != nil {
		ap.Reason = *reason
	}
	return ap, nil
}

func insertApproval(ctx context.Context, tx pgx.Tx, ap *Approval) error {
	contextJSON, err := json.Marshal(ap.Context)
	if err != nil {
		return fmt.Errorf("oversight: marshal context_package: %w", err)
	}
	return tx.QueryRow(ctx, `
		INSERT INTO approvals (
			approval_id, tenant_id, session_id, tool_use_event_id, tool_id, ask_kind,
			canonical_digest, context_package, assignee, status, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING created_at`,
		ap.ApprovalID, ap.TenantID, ap.SessionID, ap.ToolUseEventID, ap.ToolID, ap.AskKind,
		ap.CanonicalDigest, contextJSON, ap.Assignee, ap.Status, ap.ExpiresAt,
	).Scan(&ap.CreatedAt)
}

func updateApprovalStatus(ctx context.Context, tx pgx.Tx, approvalID uuid.UUID, status ApprovalStatus, decidedBy string, grantedInput []byte, reason string) error {
	_, err := tx.Exec(ctx, `
		UPDATE approvals SET status = $2, decided_at = now(), decided_by = $3, granted_input = $4, reason = $5
		WHERE approval_id = $1`,
		approvalID, status, nullIfEmpty(decidedBy), grantedInput, nullIfEmpty(reason),
	)
	if err != nil {
		return fmt.Errorf("oversight: update approval %s status: %w", approvalID, err)
	}
	return nil
}

func updateApprovalModified(ctx context.Context, tx pgx.Tx, approvalID uuid.UUID, decidedBy string, grantedInput, newDigest []byte) error {
	_, err := tx.Exec(ctx, `
		UPDATE approvals SET status = $2, decided_at = now(), decided_by = $3, granted_input = $4, canonical_digest = $5
		WHERE approval_id = $1`,
		approvalID, ApprovalGrantedModified, nullIfEmpty(decidedBy), []byte(grantedInput), newDigest,
	)
	if err != nil {
		return fmt.Errorf("oversight: update approval %s (modified): %w", approvalID, err)
	}
	return nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
