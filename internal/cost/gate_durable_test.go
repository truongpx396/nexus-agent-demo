package cost_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// fullUsage costs exactly the worst case this rig reserves: 1000 uncached input
// tokens at $3 and 1000 output tokens at $15 per million.
var fullUsage = provider.Usage{InputUncached: 1_000, OutputTokens: 1_000}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

// gateAt builds a Gate over the rig's ledger and counters, as a second process
// (or a restarted one) would, with a clock the test moves.
func (r *rig) gateAt(c *clock) *cost.Gate {
	return cost.NewGate(r.ledger, r.counters, nil, cost.GateConfig{
		MaxInputTokenEstimate: 1_000, MaxOutputTokenEstimate: 1_000,
		CacheTTL: time.Minute, Now: c.Now,
	})
}

func (r *rig) spend(g *cost.Gate) (cost.Reservation, error) {
	r.t.Helper()
	res, err := g.Reserve(context.Background(), cost.ReserveRequest{TenantID: r.tenant, SessionID: r.session, ModelID: "m", Purpose: cost.PurposeTurn})
	if err == nil {
		if rerr := g.Reconcile(context.Background(), res, fullUsage, true); rerr != nil {
			r.t.Fatalf("Reconcile: %v", rerr)
		}
	}
	return res, err
}

// A per-run ceiling is a promise about the run, not about one process. A process
// that first sees the session after a restart, or on another worker, has to count
// what the run has already spent, or the ceiling reopens every time the run moves.
func TestReserve_ASessionsRecordedSpendCountsInAProcessThatNeverSawIt(t *testing.T) {
	r := newRig(t)
	r.ledger.SeedBudget(r.tenant, cost.BudgetScopeSession, &r.session, r.usd(40_000))
	c := &clock{now: time.Now()}

	first := r.gateAt(c)
	for range 2 {
		if _, err := r.spend(first); err != nil {
			t.Fatalf("a reservation under the ceiling was refused: %v", err)
		}
	}
	if got, _ := r.ledger.SumCostRecords(context.Background(), r.tenant, cost.BudgetScopeSession, &r.session); got != 2*worstCase {
		t.Fatalf("the ledger holds %d micros for the session, want the two reconciled calls (%d)", got, 2*worstCase)
	}

	restarted := r.gateAt(c) // nothing cached: a fresh process
	if _, err := r.spend(restarted); err == nil {
		t.Fatal("a process that never saw the session reserved past its ceiling: 36,000 spent of 40,000, and 18,000 more was allowed")
	}
}

func TestSweepIdle_DropsIdleSessionStateAndTheSpendIsRebuiltFromTheLedger(t *testing.T) {
	r := newRig(t)
	r.ledger.SeedBudget(r.tenant, cost.BudgetScopeSession, &r.session, r.usd(40_000))
	c := &clock{now: time.Now()}
	g := r.gateAt(c)
	for range 2 {
		if _, err := r.spend(g); err != nil {
			t.Fatal(err)
		}
	}

	if got := g.SweepIdle(c.now.Add(-time.Hour)); got != 0 {
		t.Fatalf("a session used just now was swept (%d), want nothing evicted", got)
	}
	c.now = c.now.Add(2 * time.Hour)
	if got := g.SweepIdle(c.now.Add(-time.Hour)); got != 1 {
		t.Fatalf("SweepIdle evicted %d sessions, want the one idle for two hours", got)
	}
	if _, err := r.spend(g); err == nil {
		t.Fatal("after the sweep the session's spend was forgotten: its ceiling reopened")
	}
}

func TestReserve_ARaisedTenantCeilingCountsOnceTheCacheExpires(t *testing.T) {
	r := newRig(t)
	b := r.ledger.SeedBudget(r.tenant, cost.BudgetScopeTenant, nil, r.usd(20_000))
	c := &clock{now: time.Now()}
	g := r.gateAt(c)

	if _, err := r.spend(g); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if _, err := r.spend(g); err == nil {
		t.Fatal("a second 18,000 under a 20,000 ceiling was allowed")
	}

	r.ledger.SetCeiling(r.tenant, b.ID, r.usd(100_000)) // an operator raises the limit
	if _, err := r.spend(g); err == nil {
		t.Fatal("the raised ceiling counted before the cache expired, so the cache is not caching")
	}
	c.now = c.now.Add(2 * time.Minute)
	if _, err := r.spend(g); err != nil {
		t.Fatalf("after the cache expired the raised ceiling should apply: %v", err)
	}
}

func TestReserve_APriceBookChangeCountsOnceTheCacheExpires(t *testing.T) {
	r := newRig(t)
	r.ledger.SeedBudget(r.tenant, cost.BudgetScopeSession, &r.session, r.usd(1_000_000))
	c := &clock{now: time.Now()}
	g := r.gateAt(c)
	reserved := func() int64 {
		t.Helper()
		res, err := g.Reserve(context.Background(), cost.ReserveRequest{TenantID: r.tenant, SessionID: r.session, ModelID: "m", Purpose: cost.PurposeTurn})
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		return res.Decision.Reserved.Micros
	}
	if got := reserved(); got != worstCase {
		t.Fatalf("reserved %d, want the worst case %d", got, worstCase)
	}

	past := time.Now().Add(-time.Hour)
	r.ledger.SeedPriceBook(r.tenant, []cost.PriceBookEntry{
		{Meter: cost.MeterInputUncached, Subject: cost.WildcardSubject, Version: 1, Currency: cost.DefaultCurrency, PricePerMillionMicros: 6_000_000, EffectiveFrom: past},
		{Meter: cost.MeterOutput, Subject: cost.WildcardSubject, Version: 1, Currency: cost.DefaultCurrency, PricePerMillionMicros: 30_000_000, EffectiveFrom: past},
	})
	if got := reserved(); got != worstCase {
		t.Fatalf("reserved %d before the cache expired, want the old %d", got, worstCase)
	}
	c.now = c.now.Add(2 * time.Minute)
	if got := reserved(); got != 2*worstCase {
		t.Fatalf("reserved %d after the cache expired, want the doubled price %d", got, 2*worstCase)
	}
}

// A refresh that fails must keep serving the last good value. Otherwise a database
// blip an hour into a run becomes a cost refusal that kills it.
func TestReserve_AFailedRefreshKeepsTheLastGoodPriceBookAndBudget(t *testing.T) {
	r := newRig(t)
	r.ledger.SeedBudget(r.tenant, cost.BudgetScopeTenant, nil, r.usd(1_000_000))
	c := &clock{now: time.Now()}
	g := r.gateAt(c)
	if _, err := r.spend(g); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("database unavailable")
	r.ledger.FailOn("LoadPriceBook", boom)
	r.ledger.FailOn("GetBudget", boom)
	c.now = c.now.Add(2 * time.Minute)
	if _, err := r.spend(g); err != nil {
		t.Fatalf("a failed refresh refused a reservation the cached values allow: %v", err)
	}

	cold := r.gateAt(c) // nothing cached to fall back to: this one must still fail closed
	if _, err := r.spend(cold); err == nil {
		t.Fatal("a gate with nothing cached reserved while the ledger was down")
	}
}
