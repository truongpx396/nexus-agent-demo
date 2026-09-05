package controlplane

import (
	"context"
	"fmt"
)

// Backend interfaces LocalPort depends on — never internal/cost,
// internal/oversight, internal/audit, or internal/obs directly. Those
// packages all transitively import internal/provider (internal/cost for
// Reconcile's provider.Usage parameter; internal/oversight via kernel,
// which it imports directly per its own doc comment) — and
// tests/contract/boundaries_test.go's rule for this package walks the
// FULL transitive import graph (packages.Visit), not just direct imports,
// so depending on those packages here — even just to call a pure helper
// like cost.ParseDecimal — would drag internal/provider in right along
// with them. cmd/nexusd supplies the real adapters (composed from the
// actual services) satisfying these interfaces, translating between their
// domain types and this package's v1 shapes — the same
// nexusdOversightPort/nexusdRunCtlPort idiom internal/surfaces/rest
// already establishes for exactly this problem, applied here to the
// control-plane boundary itself.
type (
	// SessionStore backs AdmitRun's durable half: the session row (and an
	// optional session-scoped budget row), created together. Encryption
	// key material is deliberately NOT part of this interface — see
	// AdmitRun's own doc comment for why that stays a data-plane concern
	// entirely outside this boundary.
	SessionStore interface {
		CreateSession(ctx context.Context, req AdmitRunV1) (AdmitRunResultV1, error)
	}
	// BudgetBackend backs ReserveBudget/ReportCost — one reservation
	// lifecycle per pair of calls. Reservation-lifetime state (matching a
	// later ReportCost back to what Reserve actually reserved) lives in
	// the adapter satisfying this interface, never here: only the adapter
	// ever holds a real internal/cost.Reservation value, which carries
	// unexported fields no wire shape could carry anyway.
	BudgetBackend interface {
		Reserve(ctx context.Context, req ReserveBudgetV1) (ReserveBudgetResultV1, error)
		ReportCost(ctx context.Context, req ReportCostV1) error
	}
	AuditBackend interface {
		EmitReceipt(ctx context.Context, req EmitAuditReceiptV1) error
	}
	ApprovalBackend interface {
		RequestApproval(ctx context.Context, req RequestApprovalV1) (RequestApprovalResultV1, error)
	}
	ContentAccessBackend interface {
		AuthorizeContentAccess(ctx context.Context, req AuthorizeContentAccessV1) (GrantV1, error)
	}
)

// LocalPort is the one implementation of Port this single-binary demo
// ships (README task 13.15, closing production-readiness finding F15) —
// real orchestration over the four backend seams above, not a stub: it
// owns the one piece of validation logic that belongs at THIS boundary
// (AdmitRun's autonomy check) and delegates every durable effect to
// whichever backend cmd/nexusd wired in. "One process today, two later"
// (README §2): a future split replaces each backend adapter with an RPC
// client behind these same four narrow interfaces — Port's own callers
// never change.
type LocalPort struct {
	Sessions SessionStore
	Budget   BudgetBackend
	Audit    AuditBackend
	Approval ApprovalBackend
	Content  ContentAccessBackend
}

func NewLocalPort(sessions SessionStore, budget BudgetBackend, auditBackend AuditBackend, approval ApprovalBackend, content ContentAccessBackend) *LocalPort {
	return &LocalPort{Sessions: sessions, Budget: budget, Audit: auditBackend, Approval: approval, Content: content}
}

// AdmitRun validates req.Autonomy (the one check that belongs at this
// boundary, not the backend's) and, if valid, delegates the durable half
// to Sessions — including a BudgetUSD parse failure, which SessionStore's
// own implementation reports back as Admitted=false, not an error: an
// operator-supplied malformed ceiling is a normal refusal, not a system
// failure.
func (p *LocalPort) AdmitRun(ctx context.Context, req AdmitRunV1) (AdmitRunResultV1, error) {
	switch req.Autonomy {
	case "read_only", "supervised", "autonomous":
	default:
		return AdmitRunResultV1{Admitted: false, Reason: fmt.Sprintf("invalid autonomy %q", req.Autonomy)}, nil
	}
	return p.Sessions.CreateSession(ctx, req)
}

func (p *LocalPort) ReserveBudget(ctx context.Context, req ReserveBudgetV1) (ReserveBudgetResultV1, error) {
	return p.Budget.Reserve(ctx, req)
}

func (p *LocalPort) ReportCost(ctx context.Context, req ReportCostV1) error {
	return p.Budget.ReportCost(ctx, req)
}

func (p *LocalPort) EmitAuditReceipt(ctx context.Context, req EmitAuditReceiptV1) error {
	return p.Audit.EmitReceipt(ctx, req)
}

func (p *LocalPort) RequestApproval(ctx context.Context, req RequestApprovalV1) (RequestApprovalResultV1, error) {
	return p.Approval.RequestApproval(ctx, req)
}

func (p *LocalPort) AuthorizeContentAccess(ctx context.Context, req AuthorizeContentAccessV1) (GrantV1, error) {
	return p.Content.AuthorizeContentAccess(ctx, req)
}

var _ Port = (*LocalPort)(nil)
