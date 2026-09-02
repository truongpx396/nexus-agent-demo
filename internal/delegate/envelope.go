package delegate

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// CreateEnvelope reserves a delegate_fanout step's whole cohort ceiling
// BEFORE the first child starts (README task 8.13) — a plain Postgres row,
// never internal/cost's tenant-scoped Redis counter: every child's own
// turn-level draw (EnvelopeBudgetGate.Reserve, below) decrements THIS row
// instead, which is what makes "per-child reservation against the tenant
// counter is prohibited" true by construction — a fan-out never makes more
// than this one call against the shared tenant ceiling.
func CreateEnvelope(ctx context.Context, tx pgx.Tx, tenantID, planSessionID uuid.UUID, ceiling cost.Money, childCount int) (uuid.UUID, error) {
	envelopeID := uuid.New()
	_, err := tx.Exec(ctx, `
		INSERT INTO fanout_envelopes (envelope_id, tenant_id, plan_session_id, currency, ceiling_micros, remaining_micros, child_count)
		VALUES ($1,$2,$3,$4,$5,$5,$6)`,
		envelopeID, tenantID, planSessionID, ceiling.Currency, ceiling.Micros, childCount,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("delegate: create fanout envelope: %w", err)
	}
	return envelopeID, nil
}

// drawFromEnvelope atomically decrements remaining_micros by amount,
// refusing (ok=false) rather than going negative — the CHECK
// (remaining_micros >= 0) constraint backs this up at the schema level too,
// but the conditional UPDATE is what turns "refuse" into a normal, expected
// outcome instead of a constraint-violation error.
func drawFromEnvelope(ctx context.Context, tx pgx.Tx, envelopeID uuid.UUID, amount cost.Money) (ok bool, remaining cost.Money, err error) {
	var remainingMicros int64
	var currency string
	err = tx.QueryRow(ctx, `
		UPDATE fanout_envelopes SET remaining_micros = remaining_micros - $2
		WHERE envelope_id = $1 AND remaining_micros >= $2
		RETURNING remaining_micros, currency`,
		envelopeID, amount.Micros,
	).Scan(&remainingMicros, &currency)
	if err != nil {
		if err == pgx.ErrNoRows {
			// Either the envelope doesn't exist, or it exists but doesn't
			// have amount left — read the current balance to report it and
			// to distinguish "exhausted" from "no such envelope" in the
			// caller's error, without a second round trip on the common path.
			var cur int64
			var cy string
			if qerr := tx.QueryRow(ctx, `SELECT remaining_micros, currency FROM fanout_envelopes WHERE envelope_id = $1`, envelopeID).Scan(&cur, &cy); qerr != nil {
				return false, cost.Money{}, fmt.Errorf("delegate: envelope %s not found", envelopeID)
			}
			return false, cost.Money{Micros: cur, Currency: cy}, nil
		}
		return false, cost.Money{}, fmt.Errorf("delegate: draw from envelope %s: %w", envelopeID, err)
	}
	return true, cost.Money{Micros: remainingMicros, Currency: currency}, nil
}

// envelopePerCallEstimate sizes one child's worst-case per-turn reservation
// as its fair share of the whole envelope (ceiling / child_count) — Spawn's
// own use, wiring a freshly-cloned Kernel's Budget field for one fanout
// child.
func envelopePerCallEstimate(ctx context.Context, st txRunner, tenantID, envelopeID uuid.UUID) (cost.Money, string, error) {
	var ceilingMicros int64
	var currency string
	var childCount int
	err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT ceiling_micros, currency, child_count FROM fanout_envelopes WHERE envelope_id = $1`, envelopeID).
			Scan(&ceilingMicros, &currency, &childCount)
	})
	if err != nil {
		return cost.Money{}, "", fmt.Errorf("delegate: load envelope %s: %w", envelopeID, err)
	}
	if childCount <= 0 {
		childCount = 1
	}
	return cost.Money{Micros: ceilingMicros / int64(childCount), Currency: currency}, currency, nil
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
	Store           txRunner
	EnvelopeID      uuid.UUID
	TenantID        uuid.UUID
	PerCallEstimate cost.Money
	Currency        string
}

// txRunner is the one method EnvelopeBudgetGate needs from *store.Store —
// named narrowly (rather than depending on *store.Store directly) purely so
// envelope_test.go can fake it without a real Postgres pool.
type txRunner interface {
	InTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error
}

func (g *EnvelopeBudgetGate) Reserve(ctx context.Context, req cost.ReserveRequest) (cost.Reservation, error) {
	res := cost.Reservation{ID: uuid.New(), TenantID: req.TenantID, SessionID: req.SessionID, ModelID: req.ModelID}
	var ok bool
	var remaining cost.Money
	err := g.Store.InTenantTx(ctx, g.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		ok, remaining, derr = drawFromEnvelope(ctx, tx, g.EnvelopeID, g.PerCallEstimate)
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
	return g.Store.InTenantTx(ctx, g.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE fanout_envelopes SET remaining_micros = remaining_micros + $2 WHERE envelope_id = $1`, g.EnvelopeID, delta.Micros)
		return err
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
	var pb *cost.PriceBook
	err := g.Store.InTenantTx(ctx, g.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var lerr error
		pb, lerr = cost.LoadPriceBook(ctx, tx)
		return lerr
	})
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
