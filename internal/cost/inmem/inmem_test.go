package inmem_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/cost/costtest"
	"github.com/truongpx396/nexus-agent-demo/internal/cost/inmem"
)

func TestLedger_SatisfiesTheLedgerContract(t *testing.T) {
	l := inmem.NewLedger()
	costtest.RunLedger(t, costtest.LedgerSubject{
		Ledger:     l,
		NewTenant:  func(*testing.T) uuid.UUID { return uuid.New() },
		NewSession: func(*testing.T, uuid.UUID) uuid.UUID { return uuid.New() },
		SeedPriceBook: func(_ *testing.T, tenantID uuid.UUID, entries []cost.PriceBookEntry) {
			l.SeedPriceBook(tenantID, entries)
		},
		SeedBudget: func(_ *testing.T, tenantID uuid.UUID, scope cost.BudgetScope, ref *uuid.UUID, ceiling cost.Money) cost.Budget {
			return l.SeedBudget(tenantID, scope, ref, ceiling)
		},
	})
}

func TestCounters_SatisfyTheCountersContract(t *testing.T) {
	costtest.RunCounters(t, func(*testing.T) cost.Counters { return inmem.NewCounters() })
}
