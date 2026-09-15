package cost

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// Reconcile prices the real usage from a completed Provider.Stream call and
// releases the unused portion of what Reserve reserved. reported=false is
// the UNREPORTED case (task 4.7): the stream failed after the commit point
// with no trustworthy usage figures, so Reconcile charges the full
// reserved worst case instead — "an unreliable provider must not look
// free."
func (g *Gate) Reconcile(ctx context.Context, res Reservation, usage provider.Usage, reported bool) error {
	subject := res.ModelID
	if subject == "" {
		subject = WildcardSubject
	}

	var records []CostRecord
	actual := Money{Currency: g.cfg.Currency}

	if !reported {
		actual = res.Decision.Reserved
		if actual.Currency == "" {
			actual.Currency = g.cfg.Currency
		}
		records = []CostRecord{{
			Meter: MeterUnreportedReservation, Quantity: 1, Unit: "reservation",
			ModelID: res.ModelID, Unreported: true, ReservationID: &res.ID, Cost: actual,
		}}
	} else {
		pb, err := g.priceBookFor(ctx, res.TenantID)
		if err != nil {
			return fmt.Errorf("cost: reconcile: price book unavailable: %w", err)
		}
		now := time.Now()
		for _, u := range []struct {
			meter MeterID
			qty   int64
		}{
			{MeterInputUncached, int64(usage.InputUncached)},
			{MeterInputCacheRead, int64(usage.InputCacheRead)},
			{MeterInputCacheWrite, int64(usage.InputCacheWrite)},
			{MeterOutput, int64(usage.OutputTokens)},
		} {
			if u.qty == 0 {
				continue
			}
			m, ok := g.meters.Lookup(u.meter)
			if !ok {
				return fmt.Errorf("cost: reconcile: %w", errUnknownMeter(u.meter))
			}
			c, cerr := pb.Cost(u.meter, subject, u.qty, now)
			if cerr != nil {
				return fmt.Errorf("cost: reconcile: %w", cerr)
			}
			records = append(records, CostRecord{
				Meter: u.meter, Quantity: u.qty, Unit: m.Unit, ModelID: res.ModelID, ReservationID: &res.ID, Cost: c,
			})
			actual, _ = actual.Add(c)
		}
	}

	if len(records) > 0 {
		if err := g.store.InTenantTx(ctx, res.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return RecordUsage(ctx, tx, res.TenantID, res.SessionID, records)
		}); err != nil {
			return fmt.Errorf("cost: reconcile: %w", err)
		}
	}

	return g.finishReconcile(ctx, res, actual)
}

// ReconcileUsage is Reconcile with the four token counts passed as
// primitives instead of a provider.Usage value (README task 13.15): a
// caller that must not import internal/provider — internal/controlplane's
// LocalPort, closing production-readiness finding F15 — has no other way
// to report usage back through this Gate. internal/cost already imports
// internal/provider for Reconcile's own signature, so building the
// provider.Usage value here, instead of at the caller, is what keeps that
// dependency from leaking to a caller that isn't allowed to have it.
func (g *Gate) ReconcileUsage(ctx context.Context, res Reservation, inputUncached, inputCacheRead, inputCacheWrite, outputTokens int, reported bool) error {
	return g.Reconcile(ctx, res, provider.Usage{
		InputUncached: inputUncached, InputCacheRead: inputCacheRead,
		InputCacheWrite: inputCacheWrite, OutputTokens: outputTokens,
	}, reported)
}

// ReconcileEmbedding is Reconcile's counterpart for a PurposeEmbedding
// reservation (task 12.4): a single meter (MeterEmbeddingTokens) priced off
// the real token count internal/provider.EmbedUsage reports, rather than
// Reconcile's four-way chat split — an embedding call has nothing to split.
// reported=false is the same UNREPORTED case Reconcile's own doc comment
// describes: charge the full reserved worst case rather than assume a
// failed call was free.
func (g *Gate) ReconcileEmbedding(ctx context.Context, res Reservation, tokensUsed int, reported bool) error {
	subject := res.ModelID
	if subject == "" {
		subject = WildcardSubject
	}

	var records []CostRecord
	actual := Money{Currency: g.cfg.Currency}

	if !reported {
		actual = res.Decision.Reserved
		if actual.Currency == "" {
			actual.Currency = g.cfg.Currency
		}
		records = []CostRecord{{
			Meter: MeterUnreportedReservation, Quantity: 1, Unit: "reservation",
			ModelID: res.ModelID, Unreported: true, ReservationID: &res.ID, Cost: actual,
		}}
	} else if tokensUsed > 0 {
		pb, err := g.priceBookFor(ctx, res.TenantID)
		if err != nil {
			return fmt.Errorf("cost: reconcile embedding: price book unavailable: %w", err)
		}
		c, err := pb.Cost(MeterEmbeddingTokens, subject, int64(tokensUsed), time.Now())
		if err != nil {
			return fmt.Errorf("cost: reconcile embedding: %w", err)
		}
		records = []CostRecord{{
			Meter: MeterEmbeddingTokens, Quantity: int64(tokensUsed), Unit: "tokens",
			ModelID: res.ModelID, ReservationID: &res.ID, Cost: c,
		}}
		actual = c
	}

	if len(records) > 0 {
		if err := g.store.InTenantTx(ctx, res.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return RecordUsage(ctx, tx, res.TenantID, res.SessionID, records)
		}); err != nil {
			return fmt.Errorf("cost: reconcile embedding: %w", err)
		}
	}

	return g.finishReconcile(ctx, res, actual)
}

// finishReconcile is Reconcile and ReconcileEmbedding's shared tail: release
// the unused portion of what Reserve reserved (worst case minus actual)
// against whichever budget(s) backed the reservation. Both callers have
// already durably recorded their own CostRecords before reaching here —
// this only ever touches the reservation counters, never cost_records
// again.
func (g *Gate) finishReconcile(ctx context.Context, res Reservation, actual Money) error {
	if res.Decision.Kind == DecisionSkip || res.Decision.Kind == DecisionRefuseCeiling {
		return nil // nothing was reserved against a counter to release
	}

	reserved := res.Decision.Reserved
	if reserved.Currency == "" {
		reserved.Currency = g.cfg.Currency
	}
	delta, err := reserved.Sub(actual)
	if err != nil {
		return fmt.Errorf("cost: reconcile: %w", err)
	}

	if res.sessionBudget != nil {
		g.mu.Lock()
		st := g.sessionState[res.SessionID]
		if adjusted, aerr := st.spent.Sub(delta); aerr == nil {
			st.spent = adjusted
			g.sessionState[res.SessionID] = st
		}
		g.mu.Unlock()
	}
	if res.tenantBudget != nil {
		if err := g.redis.Release(ctx, res.tenantBudget.ID, res.tenantEpoch, delta.Micros); err != nil {
			log.Error().Err(err).Any("tenant_id", res.TenantID).Any("session_id", res.SessionID).Msg("cost: failed to release tenant reservation delta")
		}
	}
	return nil
}

// Record durably logs qty units of a meter with no pre-spend reservation —
// the abstract BudgetGate.Record method (README §4), used for a
// non-reservable meter (MeterSandboxSeconds, MeterToolInvocations) whose
// true cost is only knowable after the fact. Nothing in this phase calls
// it (task 4.2's "registered but unemitted"); it exists so the seam is
// real and independently testable rather than merely declared.
func (g *Gate) Record(ctx context.Context, tenantID, sessionID uuid.UUID, meter MeterID, qty int64, modelID string) error {
	m, ok := g.meters.Lookup(meter)
	if !ok {
		return errUnknownMeter(meter)
	}
	pb, err := g.priceBookFor(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("cost: record: price book unavailable: %w", err)
	}
	subject := modelID
	if subject == "" {
		subject = WildcardSubject
	}
	c, err := pb.Cost(meter, subject, qty, time.Now())
	if err != nil {
		return fmt.Errorf("cost: record: %w", err)
	}
	return g.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return RecordUsage(ctx, tx, tenantID, sessionID, []CostRecord{{Meter: meter, Quantity: qty, Unit: m.Unit, ModelID: modelID, Cost: c}})
	})
}
