package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
)

// GetBudget looks up the budget for one scope (and, for BudgetScopeSession,
// one scopeRef), returning ok=false if no such budget was ever created —
// callers treat "no budget" as "nothing to enforce here" (gate.go's `skip`
// decision), never as "ceiling of zero."
func GetBudget(ctx context.Context, tx pgx.Tx, scope cost.BudgetScope, scopeRef *uuid.UUID) (cost.Budget, bool, error) {
	var b cost.Budget
	var ceilingMicros int64
	var currency string
	row := tx.QueryRow(ctx, `
		SELECT budget_id, scope, scope_ref, currency, ceiling_micros, epoch
		FROM budgets WHERE scope = $1 AND scope_ref IS NOT DISTINCT FROM $2`,
		string(scope), scopeRef,
	)
	if err := row.Scan(&b.ID, &b.Scope, &b.ScopeRef, &currency, &ceilingMicros, &b.Epoch); err != nil {
		if err == pgx.ErrNoRows {
			return cost.Budget{}, false, nil
		}
		return cost.Budget{}, false, fmt.Errorf("get budget scope=%s: %w", scope, err)
	}
	b.Ceiling = cost.Money{Micros: ceilingMicros, Currency: currency}
	return b, true, nil
}

// CreateBudget inserts a new budget row. scope=BudgetScopeSession callers
// (internal/surfaces/rest's handleCreateRun) must pass a non-nil scopeRef
// pointing at that session; scope=BudgetScopeTenant callers pass nil.
func CreateBudget(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, scope cost.BudgetScope, scopeRef *uuid.UUID, ceiling cost.Money) (cost.Budget, error) {
	b := cost.Budget{ID: uuid.New(), Scope: scope, ScopeRef: scopeRef, Ceiling: ceiling, Epoch: 1}
	_, err := tx.Exec(ctx, `
		INSERT INTO budgets (budget_id, tenant_id, scope, scope_ref, currency, ceiling_micros, epoch)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		b.ID, tenantID, string(scope), scopeRef, ceiling.Currency, ceiling.Micros, b.Epoch,
	)
	if err != nil {
		return cost.Budget{}, fmt.Errorf("create budget scope=%s: %w", scope, err)
	}
	return b, nil
}
