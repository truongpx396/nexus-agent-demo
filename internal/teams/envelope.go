package teams

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// envelopePerCallEstimate sizes one member's worst-case per-turn
// reservation as its fair share of the whole envelope (ceiling /
// member_count) — CreateTeam's own use, wiring each freshly-cloned Kernel's
// Budget field.
func envelopePerCallEstimate(ctx context.Context, st Store, tenantID, envelopeID uuid.UUID) (cost.Money, string, error) {
	var ceiling cost.Money
	var memberCount int
	err := st.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		ceiling, memberCount, err = tx.EnvelopeShare(ctx, envelopeID)
		return err
	})
	if err != nil {
		return cost.Money{}, "", fmt.Errorf("teams: load envelope %s: %w", envelopeID, err)
	}
	if memberCount <= 0 {
		memberCount = 1
	}
	return cost.Money{Micros: ceiling.Micros / int64(memberCount), Currency: ceiling.Currency}, ceiling.Currency, nil
}

// EnvelopeBudgetGate is the kernel.BudgetGate a team member's own
// kernel.Run is wired with instead of the process's real internal/cost.Gate
// — every Reserve draws from ONE shared team_envelopes row rather than
// reserving independently against the tenant's Redis-backed ceiling (task
// 9.8). Mirrors internal/delegate.EnvelopeBudgetGate field-for-field and
// method-for-method; kept as this package's own type (rather than reused
// from internal/delegate) for the same independent-shape reason
// envelope.go's own doc comment gives for the table it draws from.
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
		ok, remaining, derr = tx.DrawFromEnvelope(ctx, g.EnvelopeID, g.PerCallEstimate)
		return derr
	})
	if err != nil {
		res.Decision = cost.Decision{Kind: cost.DecisionRefuseCeiling, Reason: "team envelope lookup failed (fail closed): " + err.Error()}
		return res, fmt.Errorf("cost: %s", res.Decision.Reason)
	}
	if !ok {
		res.Decision = cost.Decision{Kind: cost.DecisionRefuseCeiling, Reason: fmt.Sprintf(
			"team envelope %s exhausted (%s remaining, this call needs %s)", g.EnvelopeID, remaining, g.PerCallEstimate)}
		return res, fmt.Errorf("cost: %s", res.Decision.Reason)
	}
	res.Decision = cost.Decision{Kind: cost.DecisionAllow, Reason: "drawn from team budget envelope " + g.EnvelopeID.String(), Reserved: g.PerCallEstimate}
	return res, nil
}

func (g *EnvelopeBudgetGate) Reconcile(ctx context.Context, res cost.Reservation, usage provider.Usage, reported bool) error {
	actual := res.Decision.Reserved
	if reported {
		var err error
		actual, err = g.priceUsage(ctx, res.ModelID, usage)
		if err != nil {
			return fmt.Errorf("teams: envelope reconcile: %w", err)
		}
	}
	delta, err := res.Decision.Reserved.Sub(actual)
	if err != nil {
		return fmt.Errorf("teams: envelope reconcile: %w", err)
	}
	if delta.Micros <= 0 {
		return nil // reserved <= actual: nothing to return, and never draw MORE back here on an over-spend
	}
	return g.Store.InTenant(ctx, g.TenantID, func(ctx context.Context, tx Tx) error {
		return tx.ReturnToEnvelope(ctx, g.EnvelopeID, delta)
	})
}

// priceUsage reprices real usage via the tenant's own price book — mirrors
// internal/delegate.EnvelopeBudgetGate.priceUsage exactly, duplicated for
// the same reason that method's own doc comment gives (cost.Gate.Reconcile
// has no way to hand back a priced total without also touching tenant/
// session budget state this gate deliberately never draws on).
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
