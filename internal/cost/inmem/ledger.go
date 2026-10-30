// Package inmem holds the in-memory adapters for cost.Ledger and
// cost.Counters. They exist so cost.Gate's decision logic — session and
// tenant ceilings, the soft-degrade threshold, fail-closed on every
// accounting failure, reconcile releasing the unused reservation — can be
// tested without Postgres or Redis. internal/cost/costtest holds them to the
// same contract as the real adapters, which is what keeps them honest.
package inmem

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
)

// Ledger is a cost.Ledger over maps, safe for concurrent use. Tenancy is a
// map key, so one tenant can never see another's rows — the same property
// RLS gives the Postgres adapter.
type Ledger struct {
	mu         sync.Mutex
	priceBooks map[uuid.UUID][]cost.PriceBookEntry
	budgets    map[uuid.UUID][]cost.Budget
	decisions  map[uuid.UUID][]DecisionRow
	records    map[uuid.UUID][]RecordRow
	fail       map[string]error
}

// DecisionRow is one recorded budget decision.
type DecisionRow struct {
	SessionID uuid.UUID
	Decision  cost.Decision
}

// RecordRow is one recorded cost record.
type RecordRow struct {
	SessionID uuid.UUID
	Record    cost.CostRecord
}

var _ cost.Ledger = (*Ledger)(nil)

// NewLedger returns an empty Ledger.
func NewLedger() *Ledger {
	return &Ledger{
		priceBooks: map[uuid.UUID][]cost.PriceBookEntry{},
		budgets:    map[uuid.UUID][]cost.Budget{},
		decisions:  map[uuid.UUID][]DecisionRow{},
		records:    map[uuid.UUID][]RecordRow{},
		fail:       map[string]error{},
	}
}

// SeedPriceBook sets the tenant's price book.
func (l *Ledger) SeedPriceBook(tenantID uuid.UUID, entries []cost.PriceBookEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.priceBooks[tenantID] = append([]cost.PriceBookEntry(nil), entries...)
}

// SeedBudget creates a budget (epoch 1, like postgres.CreateBudget).
func (l *Ledger) SeedBudget(tenantID uuid.UUID, scope cost.BudgetScope, scopeRef *uuid.UUID, ceiling cost.Money) cost.Budget {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := cost.Budget{ID: uuid.New(), Scope: scope, ScopeRef: scopeRef, Ceiling: ceiling, Epoch: 1}
	l.budgets[tenantID] = append(l.budgets[tenantID], b)
	return b
}

// FailOn makes every later call to the named Ledger method (for example
// "RecordDecision") return err; a nil err clears it. This is how tests
// reach the Gate's fail-closed paths.
func (l *Ledger) FailOn(method string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		delete(l.fail, method)
		return
	}
	l.fail[method] = err
}

// Decisions returns the budget decisions recorded for the tenant, oldest first.
func (l *Ledger) Decisions(tenantID uuid.UUID) []DecisionRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]DecisionRow(nil), l.decisions[tenantID]...)
}

// Records returns the cost records recorded for the tenant, oldest first.
func (l *Ledger) Records(tenantID uuid.UUID) []RecordRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]RecordRow(nil), l.records[tenantID]...)
}

func (l *Ledger) LoadPriceBook(_ context.Context, tenantID uuid.UUID) (*cost.PriceBook, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.fail["LoadPriceBook"]; err != nil {
		return nil, err
	}
	return cost.NewPriceBook(append([]cost.PriceBookEntry(nil), l.priceBooks[tenantID]...)), nil
}

func (l *Ledger) GetBudget(_ context.Context, tenantID uuid.UUID, scope cost.BudgetScope, scopeRef *uuid.UUID) (cost.Budget, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.fail["GetBudget"]; err != nil {
		return cost.Budget{}, false, err
	}
	for _, b := range l.budgets[tenantID] {
		if b.Scope == scope && sameRef(b.ScopeRef, scopeRef) {
			return b, true, nil
		}
	}
	return cost.Budget{}, false, nil
}

// sameRef is SQL's IS NOT DISTINCT FROM over a nullable uuid.
func sameRef(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (l *Ledger) SumCostRecords(_ context.Context, tenantID uuid.UUID, scope cost.BudgetScope, sessionID *uuid.UUID) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.fail["SumCostRecords"]; err != nil {
		return 0, err
	}
	var total int64
	for _, r := range l.records[tenantID] {
		if scope == cost.BudgetScopeSession && (sessionID == nil || r.SessionID != *sessionID) {
			continue
		}
		total += r.Record.Cost.Micros
	}
	return total, nil
}

func (l *Ledger) RecordDecision(_ context.Context, tenantID, sessionID uuid.UUID, d cost.Decision) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.fail["RecordDecision"]; err != nil {
		return err
	}
	l.decisions[tenantID] = append(l.decisions[tenantID], DecisionRow{SessionID: sessionID, Decision: d})
	return nil
}

func (l *Ledger) RecordUsage(_ context.Context, tenantID, sessionID uuid.UUID, records []cost.CostRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.fail["RecordUsage"]; err != nil {
		return err
	}
	for _, r := range records {
		if r.ID == uuid.Nil {
			r.ID = uuid.New()
		}
		l.records[tenantID] = append(l.records[tenantID], RecordRow{SessionID: sessionID, Record: r})
	}
	return nil
}
