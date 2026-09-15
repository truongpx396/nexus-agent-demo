package cost

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// --- internal helpers ---

func (g *Gate) worstCaseCost(pb *PriceBook, modelID string, purpose Purpose) (Money, error) {
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
	in, err := pb.Cost(MeterInputUncached, subject, g.cfg.MaxInputTokenEstimate, now)
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
	if pb, ok := g.priceBooks[tenantID]; ok {
		g.mu.Unlock()
		return pb, nil
	}
	g.mu.Unlock()

	var pb *PriceBook
	err := g.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var lerr error
		pb, lerr = LoadPriceBook(ctx, tx)
		return lerr
	})
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.priceBooks[tenantID] = pb
	g.mu.Unlock()
	return pb, nil
}

func (g *Gate) sessionBudgetFor(ctx context.Context, tenantID, sessionID uuid.UUID) (*Budget, error) {
	g.mu.Lock()
	if st, ok := g.sessionState[sessionID]; ok {
		g.mu.Unlock()
		return st.budget, nil
	}
	g.mu.Unlock()

	var found *Budget
	err := g.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, ok, gerr := GetBudget(ctx, tx, BudgetScopeSession, &sessionID)
		if gerr != nil {
			return gerr
		}
		if ok {
			found = &b
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.sessionState[sessionID] = sessionCeilState{budget: found}
	g.mu.Unlock()
	return found, nil
}

func (g *Gate) tenantBudgetFor(ctx context.Context, tenantID uuid.UUID) (*Budget, error) {
	g.mu.Lock()
	if b, ok := g.tenantCache[tenantID]; ok {
		g.mu.Unlock()
		return b, nil
	}
	g.mu.Unlock()

	var found *Budget
	err := g.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, ok, gerr := GetBudget(ctx, tx, BudgetScopeTenant, nil)
		if gerr != nil {
			return gerr
		}
		if ok {
			found = &b
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.tenantCache[tenantID] = found
	g.mu.Unlock()
	return found, nil
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

	var baseline int64
	err := g.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var serr error
		baseline, serr = SumCostRecords(ctx, tx, BudgetScopeTenant, nil)
		return serr
	})
	if err != nil {
		return fmt.Errorf("sum existing cost records to arm budget %s: %w", b.ID, err)
	}
	if err := g.redis.Arm(ctx, b.ID, b.Epoch, baseline); err != nil {
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
	return g.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return RecordDecision(ctx, tx, tenantID, sessionID, d)
	})
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
