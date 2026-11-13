package cost

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// --- internal helpers ---

func (g *Gate) worstCaseCost(pb *PriceBook, modelID string, purpose Purpose, inputEstimate int64) (Money, error) {
	subject := modelID
	if subject == "" {
		subject = WildcardSubject
	}
	now := time.Now()

	// An embedding call has no output half and no cache to price around —
	// task 12.4's worst case is just its one reservable meter, sized off
	// MaxEmbeddingTokenEstimate exactly the way the chat path below is
	// sized off MaxInputTokenEstimate/MaxOutputTokenEstimate.
	if purpose == PurposeEmbedding {
		return pb.Cost(MeterEmbeddingTokens, subject, g.cfg.MaxEmbeddingTokenEstimate, now)
	}

	// Worst case assumes NO cache benefit — the whole estimated input
	// priced as MeterInputUncached, never InputCacheRead — because a
	// reservation exists to bound the call BEFORE the provider tells us
	// whether the cache actually hit.
	// The configured estimate is a floor, not the size of every prompt: a call whose
	// prompt is known to be bigger reserves for that (ReserveRequest.InputTokenEstimate).
	in, err := pb.Cost(MeterInputUncached, subject, max(g.cfg.MaxInputTokenEstimate, inputEstimate), now)
	if err != nil {
		return Money{}, err
	}
	out, err := pb.Cost(MeterOutput, subject, g.cfg.MaxOutputTokenEstimate, now)
	if err != nil {
		return Money{}, err
	}
	return in.Add(out)
}

func (g *Gate) priceBookFor(ctx context.Context, tenantID uuid.UUID) (*PriceBook, error) {
	g.mu.Lock()
	c, ok := g.priceBooks[tenantID]
	g.mu.Unlock()
	if ok && !g.stale(c.loaded) {
		return c.value, nil
	}

	pb, err := g.ledger.LoadPriceBook(ctx, tenantID)
	if err != nil {
		if ok {
			log.Warn().Err(err).Any("tenant_id", tenantID).Msg("cost: could not refresh the price book; using the last one read")
			return c.value, nil
		}
		return nil, err
	}
	g.mu.Lock()
	g.priceBooks[tenantID] = cached[*PriceBook]{value: pb, loaded: g.now()}
	g.mu.Unlock()
	return pb, nil
}

// sessionBudgetFor returns the session's ceiling, loading it (and the spend so
// far) the first time this process sees the session. The spend is summed from the
// ledger, never started at zero: a run resumed after a restart, or on another
// process, must not get a fresh ceiling.
func (g *Gate) sessionBudgetFor(ctx context.Context, tenantID, sessionID uuid.UUID) (*Budget, error) {
	g.mu.Lock()
	if st, ok := g.sessionState[sessionID]; ok {
		st.used = g.now()
		g.sessionState[sessionID] = st
		g.mu.Unlock()
		return st.budget, nil
	}
	g.mu.Unlock()

	found, err := g.budgetFor(ctx, tenantID, BudgetScopeSession, &sessionID)
	if err != nil {
		return nil, err
	}
	var spent Money
	if found != nil {
		sum, serr := g.ledger.SumCostRecords(ctx, tenantID, BudgetScopeSession, &sessionID)
		if serr != nil {
			return nil, fmt.Errorf("sum the spend already recorded for session %s: %w", sessionID, serr)
		}
		spent = Money{Micros: sum, Currency: found.Ceiling.Currency}
	}
	g.mu.Lock()
	g.sessionState[sessionID] = sessionCeilState{budget: found, spent: spent, used: g.now()}
	g.mu.Unlock()
	return found, nil
}

func (g *Gate) tenantBudgetFor(ctx context.Context, tenantID uuid.UUID) (*Budget, error) {
	g.mu.Lock()
	c, ok := g.tenantCache[tenantID]
	g.mu.Unlock()
	if ok && !g.stale(c.loaded) {
		return c.value, nil
	}

	found, err := g.budgetFor(ctx, tenantID, BudgetScopeTenant, nil)
	if err != nil {
		if ok {
			log.Warn().Err(err).Any("tenant_id", tenantID).Msg("cost: could not refresh the tenant budget; using the last one read")
			return c.value, nil
		}
		return nil, err
	}
	g.mu.Lock()
	g.tenantCache[tenantID] = cached[*Budget]{value: found, loaded: g.now()}
	g.mu.Unlock()
	return found, nil
}

// budgetFor reads one budget from the Ledger; a nil result means none exists.
func (g *Gate) budgetFor(ctx context.Context, tenantID uuid.UUID, scope BudgetScope, scopeRef *uuid.UUID) (*Budget, error) {
	b, ok, err := g.ledger.GetBudget(ctx, tenantID, scope, scopeRef)
	if err != nil || !ok {
		return nil, err
	}
	return &b, nil
}

// ensureArmed initializes budgetID's Redis epoch/spend keys the first time
// this process touches it, rehydrating the spend counter from Postgres's
// own reconciled cost_records history — never from zero (README task 4.4's
// "never 'no spend yet'") — so a cold-started or previously-flushed Redis
// recovers to the TRUE spend rather than silently re-opening the ceiling.
// A no-op if the keys already exist (luaArm), so re-arming a still-live
// budget from a second process never clobbers live state.
func (g *Gate) ensureArmed(ctx context.Context, tenantID uuid.UUID, b *Budget) error {
	g.mu.Lock()
	if g.armed[b.ID] {
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()

	baseline, err := g.ledger.SumCostRecords(ctx, tenantID, BudgetScopeTenant, nil)
	if err != nil {
		return fmt.Errorf("sum existing cost records to arm budget %s: %w", b.ID, err)
	}
	if err := g.counters.Arm(ctx, b.ID, b.Epoch, baseline); err != nil {
		return err
	}
	g.mu.Lock()
	g.armed[b.ID] = true
	g.mu.Unlock()
	return nil
}

func (g *Gate) refuse(ctx context.Context, res Reservation, budgetID *uuid.UUID, reserved Money, reason string) (Reservation, error) {
	res.Decision = Decision{Kind: DecisionRefuseCeiling, Reason: reason, BudgetID: budgetID, Reserved: Money{Currency: reserved.Currency}}
	g.bestEffortPersist(ctx, res.TenantID, res.SessionID, res.Decision)
	return res, fmt.Errorf("cost: %s", reason)
}

func (g *Gate) bestEffortPersist(ctx context.Context, tenantID, sessionID uuid.UUID, d Decision) {
	if err := g.persistDecision(ctx, tenantID, sessionID, d); err != nil {
		log.Error().Err(err).Any("tenant_id", tenantID).Any("session_id", sessionID).Any("decision", d.Kind).Msg("cost: failed to persist budget decision")
	}
}

func (g *Gate) persistDecision(ctx context.Context, tenantID, sessionID uuid.UUID, d Decision) error {
	return g.ledger.RecordDecision(ctx, tenantID, sessionID, d)
}

func decisionReason(kind DecisionKind, sessBudget, tenBudget *Budget) string {
	if kind == DecisionDegrade {
		return "reservation fit but crossed the soft threshold toward a configured ceiling"
	}
	var parts []string
	if sessBudget != nil {
		parts = append(parts, "session ceiling has room")
	}
	if tenBudget != nil {
		parts = append(parts, "tenant ceiling has room")
	}
	return strings.Join(parts, "; ")
}
