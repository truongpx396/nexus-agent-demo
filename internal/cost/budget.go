package cost

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BudgetScope names what a Budget's ceiling applies to. "session" backs the
// worker-local, per-run hard ceiling (README task 4.5 — "an agent that
// cannot run away" per-task); "tenant" backs the cross-session,
// cross-worker ceiling task 4.4's Redis-Lua epoch counter enforces.
type BudgetScope string

const (
	BudgetScopeTenant  BudgetScope = "tenant"
	BudgetScopeSession BudgetScope = "session"
)

// Budget is one row of migrations/0005_cost.sql's budgets table: a ceiling
// in exact currency micros, scoped either to a whole tenant or to one
// session. Epoch is the generation counter Reserve's Redis Lua script
// checks (gate.go/redis.go) — it only advances via an explicit
// administrative reset (no reset operation ships this phase; the column is
// the seam, the same "ships now, exercised later" pattern the rest of this
// codebase already uses for forward-referenced columns).
type Budget struct {
	ID       uuid.UUID
	Scope    BudgetScope
	ScopeRef *uuid.UUID // nil for BudgetScopeTenant; the session_id for BudgetScopeSession
	Ceiling  Money
	Epoch    int64
}

// GetBudget looks up the budget for one scope (and, for BudgetScopeSession,
// one scopeRef), returning ok=false if no such budget was ever created —
// callers treat "no budget" as "nothing to enforce here" (gate.go's `skip`
// decision), never as "ceiling of zero."
func GetBudget(ctx context.Context, tx pgx.Tx, scope BudgetScope, scopeRef *uuid.UUID) (Budget, bool, error) {
	var b Budget
	var ceilingMicros int64
	var currency string
	row := tx.QueryRow(ctx, `
		SELECT budget_id, scope, scope_ref, currency, ceiling_micros, epoch
		FROM budgets WHERE scope = $1 AND scope_ref IS NOT DISTINCT FROM $2`,
		string(scope), scopeRef,
	)
	if err := row.Scan(&b.ID, &b.Scope, &b.ScopeRef, &currency, &ceilingMicros, &b.Epoch); err != nil {
		if err == pgx.ErrNoRows {
			return Budget{}, false, nil
		}
		return Budget{}, false, fmt.Errorf("get budget scope=%s: %w", scope, err)
	}
	b.Ceiling = Money{Micros: ceilingMicros, Currency: currency}
	return b, true, nil
}

// CreateBudget inserts a new budget row. scope=BudgetScopeSession callers
// (internal/surfaces/rest's handleCreateRun) must pass a non-nil scopeRef
// pointing at that session; scope=BudgetScopeTenant callers pass nil.
func CreateBudget(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, scope BudgetScope, scopeRef *uuid.UUID, ceiling Money) (Budget, error) {
	b := Budget{ID: uuid.New(), Scope: scope, ScopeRef: scopeRef, Ceiling: ceiling, Epoch: 1}
	_, err := tx.Exec(ctx, `
		INSERT INTO budgets (budget_id, tenant_id, scope, scope_ref, currency, ceiling_micros, epoch)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		b.ID, tenantID, string(scope), scopeRef, ceiling.Currency, ceiling.Micros, b.Epoch,
	)
	if err != nil {
		return Budget{}, fmt.Errorf("create budget scope=%s: %w", scope, err)
	}
	return b, nil
}
