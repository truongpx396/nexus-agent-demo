package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/controlplane"
	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	costpg "github.com/truongpx396/nexus-agent-demo/internal/cost/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Sessions is the controlplane.SessionStore over Postgres: the durable half of
// AdmitRun. It creates the session row and, when a ceiling was asked for, its
// session-scoped budget row — in ONE transaction, because budgets.scope_ref
// has a foreign key to sessions and a caller must never observe a session
// with no way to enforce the ceiling it asked for.
type Sessions struct {
	Store *store.Store
}

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
			if _, err := costpg.CreateBudget(ctx, tx, req.TenantID, cost.BudgetScopeSession, &req.SessionID, *budgetCeiling); err != nil {
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
