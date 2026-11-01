package delegate

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// envelopePerCallEstimate sizes one child's worst-case per-turn reservation
// as its fair share of the whole envelope (ceiling / child_count) — Spawn's
// own use, wiring a freshly-cloned Kernel's Budget field for one fanout
// child.
func envelopePerCallEstimate(ctx context.Context, st Store, tenantID, envelopeID uuid.UUID) (cost.Money, string, error) {
	var ceiling cost.Money
	var childCount int
	err := st.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		ceiling, childCount, err = tx.EnvelopeShare(ctx, envelopeID)
		return err
	})
	if err != nil {
		return cost.Money{}, "", fmt.Errorf("delegate: load envelope %s: %w", envelopeID, err)
	}
	if childCount <= 0 {
		childCount = 1
	}
	return cost.Money{Micros: ceiling.Micros / int64(childCount), Currency: ceiling.Currency}, ceiling.Currency, nil
}

// EnvelopeBudgetGate is the kernel.BudgetGate a delegate_fanout child's own
// kernel.Run is wired with instead of the process's real internal/cost.Gate
// — every Reserve draws from ONE shared fanout_envelopes row rather than
// reserving independently against the tenant's Redis-backed ceiling
// (README task 8.13). Reconcile prices the REAL usage via the tenant's own
// price book (internal/cost.LoadPriceBook/PriceBook.Cost — both already
// exported for exactly this kind of external, honest reuse) and returns the
// unused portion of PerCallEstimate back to the envelope, mirroring
// cost.Gate.Reconcile's own reserve/release shape without needing any
// change to that package.
type EnvelopeBudgetGate struct {
	Store           Store
	Prices          PriceBooks
	EnvelopeID      uuid.UUID
	TenantID        uuid.UUID
	PerCallEstimate cost.Money
	Currency        string
}

func (g *EnvelopeBudgetGate) Reserve(ctx context.Context, req cost.ReserveRequest) (cost.Reservation, error) {
	res := cost.Reservation{ID: uuid.New(), TenantID: req.TenantID, SessionID: req.SessionID, ModelID: req.ModelID}
	var ok bool
	var remaining cost.Money
	err := g.Store.InTenant(ctx, g.TenantID, func(ctx context.Context, tx Tx) error {
		var derr error
		ok, remaining, derr = tx.DrawEnvelope(ctx, g.EnvelopeID, g.PerCallEstimate)
		return derr
	})
	if err != nil {
		res.Decision = cost.Decision{Kind: cost.DecisionRefuseCeiling, Reason: "fanout envelope lookup failed (fail closed): " + err.Error()}
		return res, fmt.Errorf("cost: %s", res.Decision.Reason)
	}
	if !ok {
		res.Decision = cost.Decision{Kind: cost.DecisionRefuseCeiling, Reason: fmt.Sprintf(
			"fanout envelope %s exhausted (%s remaining, this call needs %s)", g.EnvelopeID, remaining, g.PerCallEstimate)}
		return res, fmt.Errorf("cost: %s", res.Decision.Reason)
	}
	res.Decision = cost.Decision{Kind: cost.DecisionAllow, Reason: "drawn from delegation fan-out envelope " + g.EnvelopeID.String(), Reserved: g.PerCallEstimate}
	return res, nil
}

func (g *EnvelopeBudgetGate) Reconcile(ctx context.Context, res cost.Reservation, usage provider.Usage, reported bool) error {
	actual := res.Decision.Reserved
	if reported {
		var err error
		actual, err = g.priceUsage(ctx, res.ModelID, usage)
		if err != nil {
			return fmt.Errorf("delegate: envelope reconcile: %w", err)
		}
	}
	delta, err := res.Decision.Reserved.Sub(actual)
	if err != nil {
		return fmt.Errorf("delegate: envelope reconcile: %w", err)
	}
	if delta.Micros <= 0 {
		return nil // reserved <= actual: nothing to return (and never draw MORE back from an over-spend here — the reservation already fail-closed at Reserve time)
	}
	return g.Store.InTenant(ctx, g.TenantID, func(ctx context.Context, tx Tx) error {
		return tx.ReturnToEnvelope(ctx, g.EnvelopeID, delta)
	})
}

// priceUsage reprices real usage via the tenant's own price book — the same
// per-meter loop internal/cost.Gate.Reconcile runs internally, duplicated
// here (rather than exported from that package) because Gate.Reconcile's
// own signature has no way to hand back the priced total without ALSO
// touching that Reservation's (nil, here) session/tenant budget state.
func (g *EnvelopeBudgetGate) priceUsage(ctx context.Context, modelID string, usage provider.Usage) (cost.Money, error) {
	currency := g.Currency
	if currency == "" {
		currency = cost.DefaultCurrency
	}
	pb, err := g.Prices.LoadPriceBook(ctx, g.TenantID)
	if err != nil {
		return cost.Money{}, err
	}
	subject := modelID
	if subject == "" {
		subject = cost.WildcardSubject
	}
	now := time.Now()
	total := cost.Zero(currency)
	for _, u := range []struct {
		meter cost.MeterID
		qty   int64
	}{
		{cost.MeterInputUncached, int64(usage.InputUncached)},
		{cost.MeterInputCacheRead, int64(usage.InputCacheRead)},
		{cost.MeterInputCacheWrite, int64(usage.InputCacheWrite)},
		{cost.MeterOutput, int64(usage.OutputTokens)},
	} {
		if u.qty == 0 {
			continue
		}
		c, cerr := pb.Cost(u.meter, subject, u.qty, now)
		if cerr != nil {
			return cost.Money{}, cerr
		}
		total, err = total.Add(c)
		if err != nil {
			return cost.Money{}, err
		}
	}
	return total, nil
}
