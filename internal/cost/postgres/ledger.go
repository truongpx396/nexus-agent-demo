// Package postgres is the Postgres adapter for cost.Ledger (migrations/0005_cost.sql).
//
// It has two faces. Ledger implements the port the Gate uses: each method is
// one independent unit of work in its own tenant-scoped transaction. The
// package-level functions (LoadPriceBook, CreateBudget, RecordUsage, ...)
// take a caller-supplied pgx.Tx instead, for the callers that must write
// inside a larger transaction of their own — the session-plus-budget
// admission, the seed command — exactly like internal/crypto.KeyStore's
// methods: price_book, budgets, budget_decisions and cost_records all carry
// tenant_id and RLS like every other table.
package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Ledger implements cost.Ledger over Postgres.
type Ledger struct {
	st *store.Store
}

var _ cost.Ledger = (*Ledger)(nil)

// New returns a Ledger that opens its tenant-scoped transactions on st.
func New(st *store.Store) *Ledger { return &Ledger{st: st} }

func (l *Ledger) LoadPriceBook(ctx context.Context, tenantID uuid.UUID) (*cost.PriceBook, error) {
	var pb *cost.PriceBook
	err := l.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var lerr error
		pb, lerr = LoadPriceBook(ctx, tx)
		return lerr
	})
	return pb, err
}

func (l *Ledger) GetBudget(ctx context.Context, tenantID uuid.UUID, scope cost.BudgetScope, scopeRef *uuid.UUID) (cost.Budget, bool, error) {
	var (
		b  cost.Budget
		ok bool
	)
	err := l.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var gerr error
		b, ok, gerr = GetBudget(ctx, tx, scope, scopeRef)
		return gerr
	})
	return b, ok, err
}

func (l *Ledger) SumCostRecords(ctx context.Context, tenantID uuid.UUID, scope cost.BudgetScope, sessionID *uuid.UUID) (int64, error) {
	var total int64
	err := l.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var serr error
		total, serr = SumCostRecords(ctx, tx, scope, sessionID)
		return serr
	})
	return total, err
}

func (l *Ledger) RecordDecision(ctx context.Context, tenantID, sessionID uuid.UUID, d cost.Decision) error {
	return l.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return RecordDecision(ctx, tx, tenantID, sessionID, d)
	})
}

func (l *Ledger) RecordUsage(ctx context.Context, tenantID, sessionID uuid.UUID, records []cost.CostRecord) error {
	return l.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return RecordUsage(ctx, tx, tenantID, sessionID, records)
	})
}
