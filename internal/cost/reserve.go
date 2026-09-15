package cost

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Reserve estimates a worst-case cost for one upcoming model call and
// checks it against whatever budgets apply (session-scoped: local,
// synchronous, task 4.5; tenant-scoped: Redis-atomic, task 4.4), in that
// order. It ALWAYS returns a populated Reservation; the returned error is
// non-nil precisely when Decision.Kind == DecisionRefuseCeiling.
func (g *Gate) Reserve(ctx context.Context, req ReserveRequest) (Reservation, error) {
	res := Reservation{ID: uuid.New(), TenantID: req.TenantID, SessionID: req.SessionID, ModelID: req.ModelID}
	zero := Money{Currency: g.cfg.Currency}

	pb, err := g.priceBookFor(ctx, req.TenantID)
	if err != nil {
		return g.refuse(ctx, res, nil, zero, "price book unavailable (fail closed): "+err.Error())
	}
	worst, err := g.worstCaseCost(pb, req.ModelID, req.Purpose)
	if err != nil {
		return g.refuse(ctx, res, nil, zero, "cannot price a worst-case reservation (fail closed): "+err.Error())
	}

	sessBudget, err := g.sessionBudgetFor(ctx, req.TenantID, req.SessionID)
	if err != nil {
		return g.refuse(ctx, res, nil, worst, "session budget lookup failed (fail closed): "+err.Error())
	}
	tenBudget, err := g.tenantBudgetFor(ctx, req.TenantID)
	if err != nil {
		return g.refuse(ctx, res, nil, worst, "tenant budget lookup failed (fail closed): "+err.Error())
	}

	if sessBudget == nil && tenBudget == nil {
		res.Decision = Decision{Kind: DecisionSkip, Reason: "no session- or tenant-scoped budget configured", Reserved: zero}
		g.bestEffortPersist(ctx, req.TenantID, req.SessionID, res.Decision)
		return res, nil
	}

	// --- session-level: local, synchronous, no I/O beyond the one-time
	// lazy load above (task 4.5 — "a ceiling never depends on a round trip").
	var tentativeSessionSpent Money
	softHit := false
	if sessBudget != nil {
		g.mu.Lock()
		current := g.sessionState[req.SessionID].spent
		g.mu.Unlock()
		if current.Currency == "" {
			current = Money{Currency: sessBudget.Ceiling.Currency}
		}
		tentativeSessionSpent, _ = current.Add(worst)
		if tentativeSessionSpent.Micros > sessBudget.Ceiling.Micros {
			return g.refuse(ctx, res, &sessBudget.ID, worst, fmt.Sprintf(
				"session budget ceiling %s exceeded (this reservation would reach %s)", sessBudget.Ceiling, tentativeSessionSpent))
		}
		if tentativeSessionSpent.Micros*100 >= sessBudget.Ceiling.Micros*g.cfg.DegradeThresholdPercent {
			softHit = true
		}
	}

	// --- tenant-level: Redis, atomic (task 4.4).
	reservedInRedis := false
	if tenBudget != nil {
		if err := g.ensureArmed(ctx, req.TenantID, tenBudget); err != nil {
			return g.refuse(ctx, res, &tenBudget.ID, worst, "tenant budget epoch unavailable (fail closed): "+err.Error())
		}
		outcome, total, rerr := g.redis.Reserve(ctx, tenBudget.ID, tenBudget.Epoch, worst.Micros, tenBudget.Ceiling.Micros)
		if rerr != nil {
			return g.refuse(ctx, res, &tenBudget.ID, worst, "tenant budget check failed (fail closed): "+rerr.Error())
		}
		switch outcome {
		case reserveUnavailable:
			return g.refuse(ctx, res, &tenBudget.ID, worst, "tenant budget epoch unavailable (fail closed, never assumed zero spend)")
		case reserveStaleEpoch:
			return g.refuse(ctx, res, &tenBudget.ID, worst, "tenant budget epoch changed since last check (fail closed)")
		case reserveOverCeiling:
			return g.refuse(ctx, res, &tenBudget.ID, worst, fmt.Sprintf(
				"tenant budget ceiling %s exceeded (already at %s)", tenBudget.Ceiling, Money{Micros: total, Currency: tenBudget.Ceiling.Currency}))
		case reserveOK:
			reservedInRedis = true
			if total*100 >= tenBudget.Ceiling.Micros*g.cfg.DegradeThresholdPercent {
				softHit = true
			}
		}
		res.tenantEpoch = tenBudget.Epoch
	}

	deciding := sessBudget
	if deciding == nil {
		deciding = tenBudget
	}
	kind := DecisionAllow
	if softHit {
		kind = DecisionDegrade
	}
	decision := Decision{Kind: kind, Reason: decisionReason(kind, sessBudget, tenBudget), BudgetID: &deciding.ID, Reserved: worst}

	if err := g.persistDecision(ctx, req.TenantID, req.SessionID, decision); err != nil {
		// The reservation itself must not silently stand if we can't even
		// durably record that it happened — release what Redis already
		// holds and fail closed, same as any other accounting failure.
		if reservedInRedis {
			if rerr := g.redis.Release(ctx, tenBudget.ID, tenBudget.Epoch, worst.Micros); rerr != nil {
				log.Error().Err(rerr).Msg("cost: failed to roll back tenant reservation after a decision-persist failure")
			}
		}
		return g.refuse(ctx, res, &deciding.ID, worst, "failed to record budget decision (fail closed): "+err.Error())
	}

	if sessBudget != nil {
		g.mu.Lock()
		g.sessionState[req.SessionID] = sessionCeilState{budget: sessBudget, spent: tentativeSessionSpent}
		g.mu.Unlock()
	}

	res.Decision = decision
	res.sessionBudget = sessBudget
	res.tenantBudget = tenBudget
	return res, nil
}
