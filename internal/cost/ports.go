package cost

import (
	"context"

	"github.com/google/uuid"
)

// Ledger is the durable half of the Gate: the price book, the budgets, the
// running spend, and the two append-only tables the Gate writes
// (budget_decisions and cost_records). Every method is one independent unit
// of work in the tenant's own scope — the Gate's Reserve/Reconcile take no
// caller transaction (gate.go's doc comment), so neither does this port. The
// Postgres adapter (internal/cost/postgres) runs each call in its own
// tenant-scoped transaction.
type Ledger interface {
	// LoadPriceBook reads every price book entry visible to the tenant.
	LoadPriceBook(ctx context.Context, tenantID uuid.UUID) (*PriceBook, error)

	// GetBudget finds the budget for (scope, scopeRef): scopeRef is the
	// session id for BudgetScopeSession and nil for BudgetScopeTenant. ok is
	// false (with a nil error) when none exists — "no budget configured" is
	// an answer, not a failure.
	GetBudget(ctx context.Context, tenantID uuid.UUID, scope BudgetScope, scopeRef *uuid.UUID) (b Budget, ok bool, err error)

	// SumCostRecords is the reconciled spend so far, in currency-micros: the
	// whole tenant's for BudgetScopeTenant, one session's (sessionID) for
	// BudgetScopeSession. It is how a cold or flushed Counters recovers to
	// the true spend instead of zero.
	SumCostRecords(ctx context.Context, tenantID uuid.UUID, scope BudgetScope, sessionID *uuid.UUID) (int64, error)

	// RecordDecision appends one budget_decisions row.
	RecordDecision(ctx context.Context, tenantID, sessionID uuid.UUID, d Decision) error

	// RecordUsage appends the given cost_records rows in one unit of work.
	RecordUsage(ctx context.Context, tenantID, sessionID uuid.UUID, records []CostRecord) error
}

// ReserveOutcome is Counters.Reserve's typed result — deliberately distinct
// from DecisionKind (decision.go): the Gate folds ReserveUnavailable and
// ReserveOverCeiling into the same DecisionRefuseCeiling (both mean
// "refuse"), but the two are logged with different reasons.
type ReserveOutcome int

const (
	ReserveUnavailable ReserveOutcome = iota // epoch missing (never armed, or the store was flushed)
	ReserveStaleEpoch                        // epoch present but not the caller's expected generation
	ReserveOverCeiling
	ReserveOK
)

// Counters is the shared, atomic spend counter behind a tenant-scoped
// ceiling. A tenant's workers span goroutines and processes, so the
// check-then-increment-then-compare has to be one atomic operation outside
// any one of them (task 4.4); internal/cost/redis implements it with Lua
// scripts.
//
// Each budget owns an epoch (a generation marker) and a spend counter scoped
// to it. Reserve refuses to act at all if the epoch is missing or doesn't
// match — "unknown epoch = unavailable = fail closed (never 'no spend yet')"
// — and only Arm creates it, from a baseline the Gate read out of the
// Ledger, so a full flush recovers to the TRUE spend, not zero.
type Counters interface {
	// Reserve adds amount to the budget's spend if the result stays within
	// ceiling. total is the resulting spend (or, for ReserveStaleEpoch, the
	// epoch actually present; 0 for ReserveUnavailable).
	Reserve(ctx context.Context, budgetID uuid.UUID, epoch, amount, ceiling int64) (outcome ReserveOutcome, total int64, err error)

	// Release subtracts delta from the spend (a negative delta adds, covering
	// "actual cost exceeded the reservation"). A stale or missing epoch is a
	// documented no-op.
	Release(ctx context.Context, budgetID uuid.UUID, epoch, delta int64) error

	// Arm establishes the epoch and the baseline spend. It is a no-op if the
	// epoch already exists: arming must never clobber a counter another
	// still-live process is reserving against.
	Arm(ctx context.Context, budgetID uuid.UUID, epoch, baseline int64) error
}
