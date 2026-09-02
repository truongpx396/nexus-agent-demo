package teams

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// createEnvelope reserves a team's whole-roster ceiling BEFORE the first
// member starts (task 9.8) — the same shape internal/delegate/envelope.go's
// own CreateEnvelope already proves for a delegate_fanout step (task 8.13),
// scoped here to team_envelopes instead of fanout_envelopes: a plain
// Postgres row, never internal/cost's tenant-scoped Redis counter. Every
// member's own turn-level draw (EnvelopeBudgetGate.Reserve, below)
// decrements THIS row instead, which is what makes "no member draws an
// independent per-tenant reservation" true by construction.
func createEnvelope(ctx context.Context, tx pgx.Tx, tenantID, teamID uuid.UUID, ceiling cost.Money, memberCount int) (uuid.UUID, error) {
	envelopeID := uuid.New()
	_, err := tx.Exec(ctx, `
		INSERT INTO team_envelopes (envelope_id, tenant_id, team_id, currency, ceiling_micros, remaining_micros, member_count)
		VALUES ($1,$2,$3,$4,$5,$5,$6)`,
		envelopeID, tenantID, teamID, ceiling.Currency, ceiling.Micros, memberCount,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("teams: create envelope: %w", err)
	}
	return envelopeID, nil
}

// drawFromEnvelope atomically decrements remaining_micros by amount,
// refusing (ok=false) rather than going negative — mirrors
// internal/delegate/envelope.go's own drawFromEnvelope exactly; the CHECK
// (remaining_micros >= 0) constraint backs this up at the schema level too,
// but the conditional UPDATE is what turns "refuse" into a normal, expected
// outcome instead of a constraint-violation error.
func drawFromEnvelope(ctx context.Context, tx pgx.Tx, envelopeID uuid.UUID, amount cost.Money) (ok bool, remaining cost.Money, err error) {
	var remainingMicros int64
	var currency string
	err = tx.QueryRow(ctx, `
		UPDATE team_envelopes SET remaining_micros = remaining_micros - $2
		WHERE envelope_id = $1 AND remaining_micros >= $2
		RETURNING remaining_micros, currency`,
		envelopeID, amount.Micros,
	).Scan(&remainingMicros, &currency)
	if err != nil {
		if err == pgx.ErrNoRows {
			var cur int64
			var cy string
			if qerr := tx.QueryRow(ctx, `SELECT remaining_micros, currency FROM team_envelopes WHERE envelope_id = $1`, envelopeID).Scan(&cur, &cy); qerr != nil {
				return false, cost.Money{}, fmt.Errorf("teams: envelope %s not found", envelopeID)
			}
			return false, cost.Money{Micros: cur, Currency: cy}, nil
		}
		return false, cost.Money{}, fmt.Errorf("teams: draw from envelope %s: %w", envelopeID, err)
	}
	return true, cost.Money{Micros: remainingMicros, Currency: currency}, nil
}

// envelopePerCallEstimate sizes one member's worst-case per-turn
// reservation as its fair share of the whole envelope (ceiling /
// member_count) — CreateTeam's own use, wiring each freshly-cloned Kernel's
// Budget field.
func envelopePerCallEstimate(ctx context.Context, st txRunner, tenantID, envelopeID uuid.UUID) (cost.Money, string, error) {
	var ceilingMicros int64
	var currency string
	var memberCount int
	err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT ceiling_micros, currency, member_count FROM team_envelopes WHERE envelope_id = $1`, envelopeID).
			Scan(&ceilingMicros, &currency, &memberCount)
	})
	if err != nil {
		return cost.Money{}, "", fmt.Errorf("teams: load envelope %s: %w", envelopeID, err)
	}
	if memberCount <= 0 {
		memberCount = 1
	}
	return cost.Money{Micros: ceilingMicros / int64(memberCount), Currency: currency}, currency, nil
}

// txRunner is the one method EnvelopeBudgetGate needs from *store.Store —
// named narrowly (rather than depending on *store.Store directly) purely so
// this package's own tests can fake it without a real Postgres pool, the
// same reason internal/delegate/envelope.go declares its own txRunner.
type txRunner interface {
	InTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error
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
	Store           txRunner
	EnvelopeID      uuid.UUID
	TenantID        uuid.UUID
	PerCallEstimate cost.Money
	Currency        string
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
	return g.Store.InTenantTx(ctx, g.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE team_envelopes SET remaining_micros = remaining_micros + $2 WHERE envelope_id = $1`, g.EnvelopeID, delta.Micros)
		return err
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
