package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent/internal/controlplane"
	"github.com/truongpx396/nexus-agent/internal/cost"
	"github.com/truongpx396/nexus-agent/internal/eventlog"
	"github.com/truongpx396/nexus-agent/internal/store"
)

// Sessions is the controlplane.SessionStore over Postgres: the durable half of
// AdmitRun. It creates the session row and, when a ceiling was asked for, its
// session-scoped budget row — in ONE transaction, because budgets.scope_ref
// has a foreign key to sessions and a caller must never observe a session
// with no way to enforce the ceiling it asked for.
type Sessions struct {
	Store *store.Store

	// Budgets writes the session-scoped budget row in the same transaction as
	// the session. It is injected, not imported: the budgets table belongs to
	// internal/cost, and this adapter must not reach into another feature's
	// adapter. cmd/nexusd passes costpg.CreateBudget. A request that asks for a
	// ceiling while this is nil is refused, never admitted without one.
	Budgets BudgetWriter
}

// BudgetWriter creates one budget row inside the caller's transaction.
// cost/postgres.CreateBudget is the implementation.
type BudgetWriter func(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, scope cost.BudgetScope, scopeRef *uuid.UUID, ceiling cost.Money) (cost.Budget, error)

var errNoBudgetWriter = errors.New("runs: a budget ceiling was requested but no budget writer is wired")

var _ controlplane.SessionStore = Sessions{}

func (s Sessions) CreateSession(ctx context.Context, req controlplane.AdmitRunV1) (controlplane.AdmitRunResultV1, error) {
	var budgetCeiling *cost.Money
	if req.BudgetUSD != "" {
		amount, err := cost.ParseDecimal(req.BudgetUSD, cost.DefaultCurrency)
		if err != nil {
			// An operator-supplied malformed ceiling is a normal refusal, not
			// a system failure.
			return controlplane.AdmitRunResultV1{Admitted: false, Reason: "invalid budget_usd: " + err.Error()}, nil
		}
		budgetCeiling = &amount
	}

	if budgetCeiling != nil && s.Budgets == nil {
		return controlplane.AdmitRunResultV1{}, errNoBudgetWriter
	}

	sessionKey := req.SessionKey
	if sessionKey == "" {
		sessionKey = req.SessionID.String()
	}
	err := s.Store.InTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := store.CreateSession(ctx, tx, eventlog.Session{
			SessionID: req.SessionID, SessionKey: sessionKey, TenantID: req.TenantID,
			SurfaceID: req.SurfaceID, UserID: req.UserID, AgentID: uuid.Nil, AgentVersion: 1,
			HarnessDigest: req.HarnessDigest, DataLabel: req.DataLabel,
			RouteModelID: req.RouteModelID, RouteReason: req.RouteReason, AutonomyLevel: req.Autonomy,
			Conversational: req.Conversational,
		}); err != nil {
			return err
		}
		if budgetCeiling != nil {
			if _, err := s.Budgets(ctx, tx, req.TenantID, cost.BudgetScopeSession, &req.SessionID, *budgetCeiling); err != nil {
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
