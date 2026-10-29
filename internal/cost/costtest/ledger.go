// Package costtest is the contract every cost.Ledger and cost.Counters
// adapter must satisfy. The in-memory adapters (internal/cost/inmem) run it
// as plain unit tests; the Postgres and Redis adapters run the identical
// suite from tests/integration through PgBouncer as nexus_app — so the fakes
// the Gate's own tests rely on cannot drift from the real thing.
package costtest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
)

// LedgerSubject adapts one cost.Ledger implementation to the suite. The port
// is read-mostly (budgets and the price book are written by other paths: a
// session's admission, the seed command), so the suite needs the subject to
// say how to put fixtures in.
type LedgerSubject struct {
	Ledger cost.Ledger

	// NewTenant returns a tenant that exists in the adapter, empty of data.
	NewTenant func(t *testing.T) uuid.UUID
	// NewSession returns a session in that tenant (cost rows reference one).
	NewSession func(t *testing.T, tenantID uuid.UUID) uuid.UUID

	SeedPriceBook func(t *testing.T, tenantID uuid.UUID, entries []cost.PriceBookEntry)
	SeedBudget    func(t *testing.T, tenantID uuid.UUID, scope cost.BudgetScope, scopeRef *uuid.UUID, ceiling cost.Money) cost.Budget
}

// RunLedger runs the Ledger contract.
func RunLedger(t *testing.T, s LedgerSubject) {
	t.Helper()
	ctx := context.Background()
	usd := func(micros int64) cost.Money { return cost.Money{Micros: micros, Currency: cost.DefaultCurrency} }

	t.Run("an empty tenant has no price book, no budget and no spend", func(t *testing.T) {
		tenant := s.NewTenant(t)
		pb, err := s.Ledger.LoadPriceBook(ctx, tenant)
		if err != nil {
			t.Fatalf("LoadPriceBook: %v", err)
		}
		if _, err := pb.Cost(cost.MeterOutput, "any-model", 1_000, time.Now()); err == nil {
			t.Error("an empty price book priced a meter; it must fail closed, never price as free")
		}
		if _, ok, err := s.Ledger.GetBudget(ctx, tenant, cost.BudgetScopeTenant, nil); err != nil || ok {
			t.Errorf("GetBudget on an empty tenant = (ok=%v, err=%v), want (false, nil): no budget is an answer, not a failure", ok, err)
		}
		if sum, err := s.Ledger.SumCostRecords(ctx, tenant, cost.BudgetScopeTenant, nil); err != nil || sum != 0 {
			t.Errorf("SumCostRecords = (%d, %v), want (0, nil)", sum, err)
		}
	})

	t.Run("a seeded price book is loaded and prices a meter", func(t *testing.T) {
		tenant := s.NewTenant(t)
		s.SeedPriceBook(t, tenant, []cost.PriceBookEntry{{
			Meter: cost.MeterOutput, Subject: cost.WildcardSubject, Version: 1,
			Currency: cost.DefaultCurrency, PricePerMillionMicros: 15_000_000, EffectiveFrom: time.Now().Add(-time.Hour),
		}})
		pb, err := s.Ledger.LoadPriceBook(ctx, tenant)
		if err != nil {
			t.Fatalf("LoadPriceBook: %v", err)
		}
		got, err := pb.Cost(cost.MeterOutput, "any-model", 1_000_000, time.Now())
		if err != nil {
			t.Fatalf("Cost: %v", err)
		}
		if got.Micros != 15_000_000 {
			t.Errorf("1M output tokens cost %d micros, want 15000000", got.Micros)
		}
	})

	t.Run("a budget is found by scope and reference, and only by those", func(t *testing.T) {
		tenant := s.NewTenant(t)
		sessionA, sessionB := s.NewSession(t, tenant), s.NewSession(t, tenant)
		tenantBudget := s.SeedBudget(t, tenant, cost.BudgetScopeTenant, nil, usd(1_000))
		sessionBudget := s.SeedBudget(t, tenant, cost.BudgetScopeSession, &sessionA, usd(250))

		got, ok, err := s.Ledger.GetBudget(ctx, tenant, cost.BudgetScopeTenant, nil)
		if err != nil || !ok {
			t.Fatalf("tenant budget = (ok=%v, err=%v), want it found", ok, err)
		}
		if got.ID != tenantBudget.ID || got.Ceiling != usd(1_000) || got.Epoch != 1 || got.ScopeRef != nil {
			t.Errorf("tenant budget = %+v, want id=%s ceiling=%v epoch=1 scopeRef=nil", got, tenantBudget.ID, usd(1_000))
		}

		got, ok, err = s.Ledger.GetBudget(ctx, tenant, cost.BudgetScopeSession, &sessionA)
		if err != nil || !ok || got.ID != sessionBudget.ID || got.Ceiling != usd(250) {
			t.Errorf("session A budget = (%+v, ok=%v, err=%v), want %s at %v", got, ok, err, sessionBudget.ID, usd(250))
		}
		if _, ok, err := s.Ledger.GetBudget(ctx, tenant, cost.BudgetScopeSession, &sessionB); err != nil || ok {
			t.Errorf("session B has no budget but GetBudget = (ok=%v, err=%v)", ok, err)
		}
	})

	t.Run("recorded usage sums per session and per tenant", func(t *testing.T) {
		tenant := s.NewTenant(t)
		a, b := s.NewSession(t, tenant), s.NewSession(t, tenant)
		rec := func(micros int64) cost.CostRecord {
			return cost.CostRecord{Meter: cost.MeterOutput, Quantity: 1, Unit: "tokens", ModelID: "m", Cost: usd(micros)}
		}
		if err := s.Ledger.RecordUsage(ctx, tenant, a, []cost.CostRecord{rec(100), rec(250)}); err != nil {
			t.Fatalf("RecordUsage a: %v", err)
		}
		if err := s.Ledger.RecordUsage(ctx, tenant, b, []cost.CostRecord{rec(50)}); err != nil {
			t.Fatalf("RecordUsage b: %v", err)
		}
		if err := s.Ledger.RecordUsage(ctx, tenant, a, nil); err != nil {
			t.Errorf("RecordUsage with no records = %v, want nil", err)
		}

		if sum, err := s.Ledger.SumCostRecords(ctx, tenant, cost.BudgetScopeSession, &a); err != nil || sum != 350 {
			t.Errorf("session A sum = (%d, %v), want 350", sum, err)
		}
		if sum, err := s.Ledger.SumCostRecords(ctx, tenant, cost.BudgetScopeTenant, nil); err != nil || sum != 400 {
			t.Errorf("tenant sum = (%d, %v), want 400", sum, err)
		}
	})

	t.Run("a decision of every kind can be recorded", func(t *testing.T) {
		tenant := s.NewTenant(t)
		session := s.NewSession(t, tenant)
		budget := s.SeedBudget(t, tenant, cost.BudgetScopeTenant, nil, usd(1_000))
		for _, d := range []cost.Decision{
			{Kind: cost.DecisionSkip, Reason: "no budget"},
			{Kind: cost.DecisionAllow, Reason: "room", BudgetID: &budget.ID, Reserved: usd(10)},
			{Kind: cost.DecisionDegrade, Reason: "soft", BudgetID: &budget.ID, Reserved: usd(10)},
			{Kind: cost.DecisionRefuseCeiling, Reason: "over", BudgetID: &budget.ID},
		} {
			if err := s.Ledger.RecordDecision(ctx, tenant, session, d); err != nil {
				t.Errorf("RecordDecision(%s) = %v", d.Kind, err)
			}
		}
	})

	t.Run("one tenant never sees another's data", func(t *testing.T) {
		owner, other := s.NewTenant(t), s.NewTenant(t)
		session := s.NewSession(t, owner)
		s.SeedBudget(t, owner, cost.BudgetScopeTenant, nil, usd(1_000))
		s.SeedPriceBook(t, owner, []cost.PriceBookEntry{{
			Meter: cost.MeterOutput, Subject: cost.WildcardSubject, Version: 1,
			Currency: cost.DefaultCurrency, PricePerMillionMicros: 1, EffectiveFrom: time.Now().Add(-time.Hour),
		}})
		if err := s.Ledger.RecordUsage(ctx, owner, session, []cost.CostRecord{{Meter: cost.MeterOutput, Quantity: 1, Unit: "tokens", Cost: usd(999)}}); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}

		if _, ok, _ := s.Ledger.GetBudget(ctx, other, cost.BudgetScopeTenant, nil); ok {
			t.Error("the other tenant can see the owner's budget")
		}
		if sum, _ := s.Ledger.SumCostRecords(ctx, other, cost.BudgetScopeTenant, nil); sum != 0 {
			t.Errorf("the other tenant's spend = %d, want 0", sum)
		}
		pb, err := s.Ledger.LoadPriceBook(ctx, other)
		if err != nil {
			t.Fatalf("LoadPriceBook: %v", err)
		}
		if _, err := pb.Cost(cost.MeterOutput, "m", 1, time.Now()); err == nil {
			t.Error("the other tenant can price against the owner's price book")
		}
	})
}
