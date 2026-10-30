package cost_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/cost/inmem"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// Prices: $3 per million uncached input tokens, $15 per million output
// tokens, $0.10 per million embedding tokens. With the estimates below a
// worst-case chat reservation is 1000 in + 1000 out = 3,000 + 15,000 = 18,000
// micros, which keeps every ceiling in these tests a small round number.
const (
	worstCase     = 18_000
	embedEstimate = 500
)

type rig struct {
	t        *testing.T
	ledger   *inmem.Ledger
	counters *inmem.Counters
	gate     *cost.Gate
	tenant   uuid.UUID
	session  uuid.UUID
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, ledger: inmem.NewLedger(), counters: inmem.NewCounters(), tenant: uuid.New(), session: uuid.New()}
	past := time.Now().Add(-time.Hour)
	r.ledger.SeedPriceBook(r.tenant, []cost.PriceBookEntry{
		{Meter: cost.MeterInputUncached, Subject: cost.WildcardSubject, Version: 1, Currency: cost.DefaultCurrency, PricePerMillionMicros: 3_000_000, EffectiveFrom: past},
		{Meter: cost.MeterOutput, Subject: cost.WildcardSubject, Version: 1, Currency: cost.DefaultCurrency, PricePerMillionMicros: 15_000_000, EffectiveFrom: past},
		{Meter: cost.MeterEmbeddingTokens, Subject: cost.WildcardSubject, Version: 1, Currency: cost.DefaultCurrency, PricePerMillionMicros: 100_000, EffectiveFrom: past},
	})
	r.gate = r.newGate(r.counters)
	return r
}

func (r *rig) newGate(c cost.Counters) *cost.Gate {
	cfg := cost.GateConfig{MaxInputTokenEstimate: 1_000, MaxOutputTokenEstimate: 1_000, MaxEmbeddingTokenEstimate: embedEstimate}
	if c == nil {
		return cost.NewGate(r.ledger, nil, nil, cfg)
	}
	return cost.NewGate(r.ledger, c, nil, cfg)
}

func (r *rig) usd(micros int64) cost.Money {
	return cost.Money{Micros: micros, Currency: cost.DefaultCurrency}
}

func (r *rig) reserve() (cost.Reservation, error) {
	r.t.Helper()
	return r.gate.Reserve(context.Background(), cost.ReserveRequest{TenantID: r.tenant, SessionID: r.session, ModelID: "m", Purpose: cost.PurposeTurn})
}

func (r *rig) lastDecision() cost.Decision {
	r.t.Helper()
	ds := r.ledger.Decisions(r.tenant)
	if len(ds) == 0 {
		r.t.Fatal("no budget decision was recorded")
	}
	return ds[len(ds)-1].Decision
}

func (r *rig) mustReserve(want cost.DecisionKind) cost.Reservation {
	r.t.Helper()
	res, err := r.reserve()
	if err != nil {
		r.t.Fatalf("Reserve: %v", err)
	}
	if res.Decision.Kind != want {
		r.t.Fatalf("Reserve decided %s (%s), want %s", res.Decision.Kind, res.Decision.Reason, want)
	}
	return res
}

func (r *rig) mustRefuse() cost.Reservation {
	r.t.Helper()
	res, err := r.reserve()
	if err == nil {
		r.t.Fatalf("Reserve = %s, want a refusal", res.Decision.Kind)
	}
	if res.Decision.Kind != cost.DecisionRefuseCeiling {
		r.t.Fatalf("Reserve returned an error but decided %s, want %s", res.Decision.Kind, cost.DecisionRefuseCeiling)
	}
	return res
}

func TestReserve_WithNoBudgetIsSkippedButStillRecorded(t *testing.T) {
	r := newRig(t)
	res := r.mustReserve(cost.DecisionSkip)
	if res.ID == uuid.Nil || res.TenantID != r.tenant || res.SessionID != r.session {
		t.Errorf("reservation = %+v, want it populated even when skipped", res)
	}
	if d := r.lastDecision(); d.Kind != cost.DecisionSkip {
		t.Errorf("recorded decision = %s, want %s", d.Kind, cost.DecisionSkip)
	}
}

func TestReserve_SessionCeilingAllowsThenDegradesThenRefuses(t *testing.T) {
	r := newRig(t)
	r.ledger.SeedBudget(r.tenant, cost.BudgetScopeSession, &r.session, r.usd(40_000))

	first := r.mustReserve(cost.DecisionAllow)
	if first.Decision.Reserved != r.usd(worstCase) {
		t.Errorf("reserved %v, want the worst case %v", first.Decision.Reserved, r.usd(worstCase))
	}
	r.mustReserve(cost.DecisionDegrade) // 36,000 of 40,000 crosses the 80% soft threshold
	refused := r.mustRefuse()           // 54,000 would pass the ceiling
	if refused.Decision.Reserved.Micros != 0 {
		t.Errorf("a refusal reserved %v, want nothing", refused.Decision.Reserved)
	}
	if d := r.lastDecision(); d.Kind != cost.DecisionRefuseCeiling {
		t.Errorf("recorded decision = %s, want the refusal to be recorded too", d.Kind)
	}
	if got := len(r.ledger.Decisions(r.tenant)); got != 3 {
		t.Errorf("%d decisions recorded, want 3 (every gate resolution)", got)
	}
}

func TestReserve_TenantCeilingIsEnforcedThroughTheCounters(t *testing.T) {
	r := newRig(t)
	b := r.ledger.SeedBudget(r.tenant, cost.BudgetScopeTenant, nil, r.usd(40_000))

	r.mustReserve(cost.DecisionAllow)
	r.mustReserve(cost.DecisionDegrade)
	r.mustRefuse()

	if spend, _ := r.counters.Spend(b.ID); spend != 2*worstCase {
		t.Errorf("counter spend = %d, want %d: the refused third reservation must leave no trace", spend, 2*worstCase)
	}
}

func TestReserve_TenantCeilingArmsFromTheLedgerNeverFromZero(t *testing.T) {
	r := newRig(t)
	b := r.ledger.SeedBudget(r.tenant, cost.BudgetScopeTenant, nil, r.usd(40_000))
	other := uuid.New()
	_ = r.ledger.RecordUsage(context.Background(), r.tenant, other, []cost.CostRecord{{Meter: cost.MeterOutput, Quantity: 1, Unit: "tokens", Cost: r.usd(30_000)}})

	// 30,000 already spent + an 18,000 reservation passes the 40,000 ceiling:
	// had the counter been armed at zero, this would have been allowed.
	r.mustRefuse()
	if spend, _ := r.counters.Spend(b.ID); spend != 30_000 {
		t.Errorf("counter armed at %d, want the ledger's 30000", spend)
	}
}

func TestReserve_FlushedCountersFailClosedRatherThanReadAsZero(t *testing.T) {
	r := newRig(t)
	r.ledger.SeedBudget(r.tenant, cost.BudgetScopeTenant, nil, r.usd(1_000_000))
	r.mustReserve(cost.DecisionAllow)

	r.counters.Flush() // Redis lost its keys mid-flight
	res := r.mustRefuse()
	if res.Decision.Reason == "" {
		t.Error("a fail-closed refusal must say why")
	}
}

func TestReserve_FailsClosedWhenAccountingIsUnavailable(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name  string
		setup func(r *rig)
	}{
		{"the price book cannot be loaded", func(r *rig) { r.ledger.FailOn("LoadPriceBook", boom) }},
		{"the budget lookup fails", func(r *rig) { r.ledger.FailOn("GetBudget", boom) }},
		{"the tenant counters are unreachable", func(r *rig) {
			r.ledger.SeedBudget(r.tenant, cost.BudgetScopeTenant, nil, r.usd(1_000_000))
			r.counters.FailWith(boom)
		}},
		{"a tenant budget exists but no counters are wired", func(r *rig) {
			r.ledger.SeedBudget(r.tenant, cost.BudgetScopeTenant, nil, r.usd(1_000_000))
			r.gate = r.newGate(nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			tc.setup(r)
			r.mustRefuse()
		})
	}
}

func TestReserve_ARecordingFailureRollsBackTheCounterReservation(t *testing.T) {
	r := newRig(t)
	b := r.ledger.SeedBudget(r.tenant, cost.BudgetScopeTenant, nil, r.usd(1_000_000))
	r.mustReserve(cost.DecisionAllow) // arms the counter and holds 18,000

	r.ledger.FailOn("RecordDecision", errors.New("disk full"))
	r.mustRefuse()

	if spend, _ := r.counters.Spend(b.ID); spend != worstCase {
		t.Errorf("counter spend = %d, want %d: a reservation that could not be recorded must be released, not left held", spend, worstCase)
	}
}

func TestReserve_PriceBookIsLoadedOncePerTenant(t *testing.T) {
	r := newRig(t)
	r.mustReserve(cost.DecisionSkip)
	r.ledger.FailOn("LoadPriceBook", errors.New("must not be asked again"))
	r.mustReserve(cost.DecisionSkip)
}

func TestReserve_EmbeddingsAreSizedFromTheirOwnEstimate(t *testing.T) {
	r := newRig(t)
	r.ledger.SeedBudget(r.tenant, cost.BudgetScopeSession, &r.session, r.usd(1_000_000))
	res, err := r.gate.Reserve(context.Background(), cost.ReserveRequest{TenantID: r.tenant, SessionID: r.session, ModelID: "e", Purpose: cost.PurposeEmbedding})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if want := r.usd(embedEstimate * 100_000 / 1_000_000); res.Decision.Reserved != want {
		t.Errorf("embedding reservation = %v, want %v", res.Decision.Reserved, want)
	}
}

func TestReconcile_ChargesActualUsageAndReleasesTheRest(t *testing.T) {
	r := newRig(t)
	b := r.ledger.SeedBudget(r.tenant, cost.BudgetScopeTenant, nil, r.usd(1_000_000))
	res := r.mustReserve(cost.DecisionAllow)

	// 100 uncached in = 300 micros, 10 out = 150 micros.
	if err := r.gate.Reconcile(context.Background(), res, provider.Usage{InputUncached: 100, OutputTokens: 10}, true); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	recs := r.ledger.Records(r.tenant)
	if len(recs) != 2 {
		t.Fatalf("%d cost records, want 2 (one per priced meter)", len(recs))
	}
	var total int64
	for _, rr := range recs {
		total += rr.Record.Cost.Micros
		if rr.Record.ReservationID == nil || *rr.Record.ReservationID != res.ID || rr.Record.Unreported {
			t.Errorf("record %+v, want it tied to the reservation and marked reported", rr.Record)
		}
	}
	if total != 450 {
		t.Errorf("recorded %d micros, want 450", total)
	}
	if spend, _ := r.counters.Spend(b.ID); spend != 450 {
		t.Errorf("counter spend = %d, want 450: everything reserved beyond the actual cost is released", spend)
	}
}

func TestReconcile_UnreportedUsageChargesTheFullReservation(t *testing.T) {
	r := newRig(t)
	b := r.ledger.SeedBudget(r.tenant, cost.BudgetScopeTenant, nil, r.usd(1_000_000))
	res := r.mustReserve(cost.DecisionAllow)

	if err := r.gate.Reconcile(context.Background(), res, provider.Usage{}, false); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	recs := r.ledger.Records(r.tenant)
	if len(recs) != 1 || !recs[0].Record.Unreported || recs[0].Record.Cost.Micros != worstCase {
		t.Fatalf("records = %+v, want one unreported record at the full %d: an unreliable provider must not look free", recs, worstCase)
	}
	if spend, _ := r.counters.Spend(b.ID); spend != worstCase {
		t.Errorf("counter spend = %d, want %d: nothing is released when usage is unknown", spend, worstCase)
	}
}

func TestReconcile_FreesSessionHeadroomForTheNextReservation(t *testing.T) {
	r := newRig(t)
	r.ledger.SeedBudget(r.tenant, cost.BudgetScopeSession, &r.session, r.usd(20_000))
	res := r.mustReserve(cost.DecisionDegrade) // 18,000 of 20,000

	if err := r.gate.Reconcile(context.Background(), res, provider.Usage{InputUncached: 100, OutputTokens: 10}, true); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Spent is now 450 of 20,000, so another 18,000 fits. Without the
	// release it would be 36,000 and refused.
	r.mustReserve(cost.DecisionDegrade)
}

func TestReconcile_ASkippedReservationRecordsUsageButReleasesNothing(t *testing.T) {
	r := newRig(t)
	res := r.mustReserve(cost.DecisionSkip)
	if err := r.gate.Reconcile(context.Background(), res, provider.Usage{OutputTokens: 10}, true); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := len(r.ledger.Records(r.tenant)); got != 1 {
		t.Errorf("%d cost records, want 1: spend is metered even when no budget is configured", got)
	}
}

func TestReconcile_ARecordingFailureIsReturned(t *testing.T) {
	r := newRig(t)
	res := r.mustReserve(cost.DecisionSkip)
	r.ledger.FailOn("RecordUsage", errors.New("disk full"))
	if err := r.gate.Reconcile(context.Background(), res, provider.Usage{OutputTokens: 10}, true); err == nil {
		t.Error("Reconcile swallowed a failure to record usage")
	}
}
