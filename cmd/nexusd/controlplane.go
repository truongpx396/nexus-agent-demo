package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/controlplane"
	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/oversight"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// This file is the composition root for internal/controlplane.Port (README
// task 13.15, closing production-readiness finding F15) — five thin
// adapters, one per backend interface controlplane.LocalPort depends on,
// each wrapping the same real service every other Port in this file
// already wraps. controlplane itself cannot import internal/{cost,
// oversight,audit,obs} (they transitively reach internal/provider, which
// tests/contract/boundaries_test.go's own rule forbids for that package),
// so the translation between their domain types and controlplane's v1
// shapes lives here instead — the same nexusdOversightPort/
// nexusdRunCtlPort idiom this file already uses for every other seam.

// nexusdSessionStore implements controlplane.SessionStore.
type nexusdSessionStore struct {
	store *store.Store
}

func (a *nexusdSessionStore) CreateSession(ctx context.Context, req controlplane.AdmitRunV1) (controlplane.AdmitRunResultV1, error) {
	var budgetCeiling *cost.Money
	if req.BudgetUSD != "" {
		amount, err := cost.ParseDecimal(req.BudgetUSD, cost.DefaultCurrency)
		if err != nil {
			return controlplane.AdmitRunResultV1{Admitted: false, Reason: "invalid budget_usd: " + err.Error()}, nil
		}
		budgetCeiling = &amount
	}

	err := a.store.InTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := store.CreateSession(ctx, tx, store.Session{
			SessionID: req.SessionID, SessionKey: req.SessionID.String(), TenantID: req.TenantID,
			SurfaceID: req.SurfaceID, UserID: req.UserID, AgentID: uuid.Nil, AgentVersion: 1,
			HarnessDigest: req.HarnessDigest, DataLabel: req.DataLabel,
			RouteModelID: req.RouteModelID, RouteReason: req.RouteReason, AutonomyLevel: req.Autonomy,
		}); err != nil {
			return err
		}
		if budgetCeiling != nil {
			// budgets.scope_ref has an FK to sessions — created in the SAME
			// transaction as the session row above, so a caller never
			// observes a session with no way to enforce the ceiling it
			// asked for.
			if _, err := cost.CreateBudget(ctx, tx, req.TenantID, cost.BudgetScopeSession, &req.SessionID, *budgetCeiling); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return controlplane.AdmitRunResultV1{}, err
	}
	return controlplane.AdmitRunResultV1{Admitted: true}, nil
}

// nexusdBudgetBackend implements controlplane.BudgetBackend. reservations
// holds each Reserve call's real cost.Reservation (unexported fields and
// all) keyed by its id, so a later ReportCost naming that id can
// reconcile against the same reservation Reserve actually made — the
// state a real RPC-based control-plane split would need a server-side
// session for; here it's just a mutex-protected map on this one adapter.
type nexusdBudgetBackend struct {
	gate *cost.Gate

	mu           sync.Mutex
	reservations map[uuid.UUID]cost.Reservation
}

func newNexusdBudgetBackend(gate *cost.Gate) *nexusdBudgetBackend {
	return &nexusdBudgetBackend{gate: gate, reservations: make(map[uuid.UUID]cost.Reservation)}
}

func (a *nexusdBudgetBackend) Reserve(ctx context.Context, req controlplane.ReserveBudgetV1) (controlplane.ReserveBudgetResultV1, error) {
	res, err := a.gate.Reserve(ctx, cost.ReserveRequest{
		TenantID: req.TenantID, SessionID: req.SessionID, ModelID: req.ModelID, Purpose: cost.Purpose(req.Purpose),
	})
	if err != nil {
		return controlplane.ReserveBudgetResultV1{}, err
	}
	a.mu.Lock()
	a.reservations[res.ID] = res
	a.mu.Unlock()
	return controlplane.ReserveBudgetResultV1{
		ReservationID: res.ID, Decision: string(res.Decision.Kind),
		Reason: res.Decision.Reason, ReservedUSD: res.Decision.Reserved.String(),
	}, nil
}

func (a *nexusdBudgetBackend) ReportCost(ctx context.Context, req controlplane.ReportCostV1) error {
	a.mu.Lock()
	res, ok := a.reservations[req.ReservationID]
	if ok {
		delete(a.reservations, req.ReservationID)
	}
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("nexusd: report cost: unknown reservation %s (not reserved through this process, or already reported)", req.ReservationID)
	}
	return a.gate.ReconcileUsage(ctx, res, req.InputUncached, req.InputCacheRead, req.InputCacheWrite, req.OutputTokens, req.Reported)
}

// nexusdAuditBackend implements controlplane.AuditBackend.
type nexusdAuditBackend struct {
	store *store.Store
	chain *audit.Chain
}

func (a *nexusdAuditBackend) EmitReceipt(ctx context.Context, req controlplane.EmitAuditReceiptV1) error {
	return a.store.InTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := a.chain.Append(ctx, tx, req.TenantID, req.SessionID, req.Seq, req.EventID, req.EventType, req.PayloadDigest)
		return err
	})
}

// nexusdApprovalBackend implements controlplane.ApprovalBackend.
type nexusdApprovalBackend struct {
	approvals *oversight.Approvals
}

func (a *nexusdApprovalBackend) RequestApproval(ctx context.Context, req controlplane.RequestApprovalV1) (controlplane.RequestApprovalResultV1, error) {
	ap, err := a.approvals.Create(ctx, oversight.CreateApprovalRequest{
		TenantID: req.TenantID, SessionID: req.SessionID, ToolUseEventID: req.ToolUseEventID,
		ToolID: req.ToolID, AskKind: req.AskKind, CanonicalDigest: req.CanonicalDigest,
		Context: oversight.ContextPackage{ToolID: req.ToolID, EffectClass: req.EffectClass, Input: req.Input},
	})
	if err != nil {
		return controlplane.RequestApprovalResultV1{}, err
	}
	return controlplane.RequestApprovalResultV1{ApprovalID: ap.ApprovalID, ExpiresAt: ap.ExpiresAt}, nil
}

// nexusdContentAccessBackend implements controlplane.ContentAccessBackend.
type nexusdContentAccessBackend struct {
	grants *obs.Grants
}

func (a *nexusdContentAccessBackend) AuthorizeContentAccess(ctx context.Context, req controlplane.AuthorizeContentAccessV1) (controlplane.GrantV1, error) {
	grant, err := a.grants.RequestGrant(ctx, req.TenantID, req.SessionID, req.GranteeID, req.Reason, req.TTL)
	if err != nil {
		return controlplane.GrantV1{}, err
	}
	return controlplane.GrantV1{GrantID: grant.GrantID, ExpiresAt: grant.ExpiresAt}, nil
}

// newControlPlane composes the five adapters above into the one
// controlplane.Port this process serves — see this file's own top-of-file
// comment for why the translation lives here rather than inside
// internal/controlplane itself.
func newControlPlane(st *store.Store, gate *cost.Gate, chain *audit.Chain, approvals *oversight.Approvals, grants *obs.Grants) *controlplane.LocalPort {
	return controlplane.NewLocalPort(
		&nexusdSessionStore{store: st},
		newNexusdBudgetBackend(gate),
		&nexusdAuditBackend{store: st, chain: chain},
		&nexusdApprovalBackend{approvals: approvals},
		&nexusdContentAccessBackend{grants: grants},
	)
}
