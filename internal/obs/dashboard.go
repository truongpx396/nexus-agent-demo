package obs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// GoldenSignals is the go-live dashboard (README task 10.12) — every field
// this codebase's own constitution names as what a real deployment must be
// able to see without ever reading conversation content. Every query in
// ComputeGoldenSignals reads structure only (a status column, a decision
// kind, a meter quantity, a timestamp) — never a payload, the same
// content-free discipline Filter enforces for spans.
type GoldenSignals struct {
	TotalTerminalSessions int
	// CompletionRateByReason is terminal_reason -> (count / TotalTerminalSessions).
	CompletionRateByReason map[string]float64
	// CostCeilingBreachRate is the fraction of budget_decisions resolved
	// refuse_ceiling (internal/cost.DecisionRefuseCeiling).
	CostCeilingBreachRate float64
	// StuckRate is the fraction of terminal sessions that ended
	// stuck_terminated (kernel.ReasonStuckTerminated).
	StuckRate float64
	// CacheReadRate mirrors internal/promptctx.CacheReadRate's own formula,
	// computed from the durable cost_records meters (internal/cost.
	// MeterInputCacheRead etc.) rather than an in-memory Usage slice, since
	// a dashboard reads across many sessions after the fact.
	CacheReadRate float64
	// ApprovalP50DecisionMS / ApprovalP95DecisionMS are decided_at-created_at
	// latency percentiles across every DECIDED approval (pending ones have
	// no decided_at and are excluded, not counted as zero-latency).
	ApprovalP50DecisionMS int64
	ApprovalP95DecisionMS int64
	// ApprovalMismatchRate is approval_mismatch events divided by decided
	// approvals — task 5.7's typed refusal, surfaced as a rate.
	ApprovalMismatchRate float64
	// UnresolvedInFlightClaims is claims still `in_flight` older than the
	// staleness window ComputeGoldenSignals was called with — a fresh
	// in-flight claim is normal (an effect mid-flight); an OLD one is what
	// task 6.6's "resolved by probe or human, never silently discarded"
	// exists to catch.
	UnresolvedInFlightClaims int
	// TelemetryAttrDropRate comes from a caller-supplied DropTracker, never
	// computed here — see DropTracker's own doc comment for why this one
	// signal can't be reconstructed from Postgres after the fact.
	TelemetryAttrDropRate float64
	// HeldOutGap is a caller-supplied evals.Gate result (task 10.6) — obs
	// stays a leaf with respect to evals (an eval run is a release-gate
	// concern, not a runtime-observability one), so the caller assembling
	// a full go-live dashboard passes this in rather than obs computing it
	// itself. Nil means "not measured this call."
	HeldOutGap *float64
	// TaintTransitionRate is the fraction of terminal sessions whose durable
	// log carries at least one taint_transition event (store.
	// EventTaintTransition — internal/delegate/resolve.go's delegation-copy
	// fold and internal/teams/board.go's board-card-read fold, constitution
	// Principle V's Rule of Two). Deliberately reads the event log rather
	// than sessions.taint_state: that column is still an unpopulated
	// placeholder for a later projection phase (internal/tools/pipeline.go's
	// own doc comment — Rule-of-Two state today lives in a process-lifetime
	// cache, not a replayed projection), while taint_transition events are
	// already durably appended today. Same "not itself bad, but the first
	// place a regression shows up" read as CompletionRateByReason.
	TaintTransitionRate float64
	// SessionsBySurface is total session count grouped by originating
	// surface_id (migrations/0002_sessions.sql — web/telegram/zalo/email/
	// cli/api, docs/agentic-capabilities.md's multi-surface delivery). A
	// count, not a rate: there is no "bad" direction, just channel adoption/
	// distribution, the same shape as ToolCallCounts's per-tool breakdown.
	SessionsBySurface map[string]int
}

// ComputeGoldenSignals queries every signal above out of tenantID's own
// tenant-scoped rows (internal/store.Store.InTenantTx — the one sanctioned
// scoping call, same as every other read in this codebase). drops and
// heldOutGap may be nil; a nil drops leaves TelemetryAttrDropRate at 0
// rather than erroring, since not every caller wires an exporter through a
// DropTracker.
func ComputeGoldenSignals(ctx context.Context, st *store.Store, tenantID uuid.UUID, staleClaimAfter time.Duration, drops *DropTracker, heldOutGap *float64) (GoldenSignals, error) {
	g := GoldenSignals{HeldOutGap: heldOutGap}
	if drops != nil {
		g.TelemetryAttrDropRate = drops.Rate()
	}

	err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := queryCompletionByReason(ctx, tx, &g); err != nil {
			return err
		}
		if err := queryCostCeilingBreachRate(ctx, tx, &g); err != nil {
			return err
		}
		if err := queryCacheReadRate(ctx, tx, &g); err != nil {
			return err
		}
		if err := queryApprovalSignals(ctx, tx, &g); err != nil {
			return err
		}
		if err := queryUnresolvedClaims(ctx, tx, &g, staleClaimAfter); err != nil {
			return err
		}
		if err := queryTaintTransitionRate(ctx, tx, &g); err != nil {
			return err
		}
		return querySessionsBySurface(ctx, tx, &g)
	})
	return g, err
}

func queryCompletionByReason(ctx context.Context, tx pgx.Tx, g *GoldenSignals) error {
	rows, err := tx.Query(ctx, `
		SELECT terminal_reason, count(*)
		FROM sessions
		WHERE terminal_reason IS NOT NULL
		GROUP BY terminal_reason`)
	if err != nil {
		return fmt.Errorf("obs: query terminal reasons: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	total := 0
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			return fmt.Errorf("obs: scan terminal reason: %w", err)
		}
		counts[reason] = n
		total += n
	}
	if err := rows.Err(); err != nil {
		return err
	}

	g.TotalTerminalSessions = total
	g.CompletionRateByReason = make(map[string]float64, len(counts))
	for reason, n := range counts {
		g.CompletionRateByReason[reason] = safeDiv(n, total)
	}
	// "stuck_terminated" mirrors kernel.ReasonStuckTerminated's string form
	// (kernel/terminal.go) without importing kernel — internal/obs must stay
	// a leaf with respect to it (kernel's own import allowlist runs the
	// other direction: kernel may import internal/obs, never the reverse),
	// so this compares against the plain text this column actually stores,
	// the same as any other tenant read in this codebase.
	g.StuckRate = safeDiv(counts["stuck_terminated"], total)
	return nil
}

func queryCostCeilingBreachRate(ctx context.Context, tx pgx.Tx, g *GoldenSignals) error {
	var total, refused int
	err := tx.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE decision = 'refuse_ceiling')
		FROM budget_decisions`,
	).Scan(&total, &refused)
	if err != nil {
		return fmt.Errorf("obs: query budget decisions: %w", err)
	}
	g.CostCeilingBreachRate = safeDiv(refused, total)
	return nil
}

func queryCacheReadRate(ctx context.Context, tx pgx.Tx, g *GoldenSignals) error {
	var uncached, cacheRead, cacheWrite int64
	err := tx.QueryRow(ctx, `
		SELECT
			coalesce(sum(quantity) FILTER (WHERE meter = 'input_uncached'), 0),
			coalesce(sum(quantity) FILTER (WHERE meter = 'input_cache_read'), 0),
			coalesce(sum(quantity) FILTER (WHERE meter = 'input_cache_write'), 0)
		FROM cost_records`,
	).Scan(&uncached, &cacheRead, &cacheWrite)
	if err != nil {
		return fmt.Errorf("obs: query cost records: %w", err)
	}
	totalInput := uncached + cacheRead + cacheWrite
	if totalInput > 0 {
		g.CacheReadRate = float64(cacheRead) / float64(totalInput)
	}
	return nil
}

func queryApprovalSignals(ctx context.Context, tx pgx.Tx, g *GoldenSignals) error {
	var p50, p95 *float64
	err := tx.QueryRow(ctx, `
		SELECT
			percentile_cont(0.5)  WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (decided_at - created_at)) * 1000),
			percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (decided_at - created_at)) * 1000)
		FROM approvals WHERE decided_at IS NOT NULL`,
	).Scan(&p50, &p95)
	if err != nil {
		return fmt.Errorf("obs: query approval latency: %w", err)
	}
	if p50 != nil {
		g.ApprovalP50DecisionMS = int64(*p50)
	}
	if p95 != nil {
		g.ApprovalP95DecisionMS = int64(*p95)
	}

	var decided, mismatches int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE decided_at IS NOT NULL`).Scan(&decided); err != nil {
		return fmt.Errorf("obs: count decided approvals: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = $1`, string(store.EventApprovalMismatch)).Scan(&mismatches); err != nil {
		return fmt.Errorf("obs: count approval_mismatch events: %w", err)
	}
	g.ApprovalMismatchRate = safeDiv(mismatches, decided)
	return nil
}

func queryUnresolvedClaims(ctx context.Context, tx pgx.Tx, g *GoldenSignals, staleAfter time.Duration) error {
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM claims
		WHERE status = 'in_flight' AND created_at < now() - make_interval(secs => $1)`,
		staleAfter.Seconds(),
	).Scan(&g.UnresolvedInFlightClaims)
	if err != nil {
		return fmt.Errorf("obs: count unresolved claims: %w", err)
	}
	return nil
}

func queryTaintTransitionRate(ctx context.Context, tx pgx.Tx, g *GoldenSignals) error {
	var sessionsWithTransition int
	err := tx.QueryRow(ctx,
		`SELECT count(DISTINCT session_id) FROM events WHERE type = $1`,
		string(store.EventTaintTransition),
	).Scan(&sessionsWithTransition)
	if err != nil {
		return fmt.Errorf("obs: count taint_transition sessions: %w", err)
	}
	g.TaintTransitionRate = safeDiv(sessionsWithTransition, g.TotalTerminalSessions)
	return nil
}

func querySessionsBySurface(ctx context.Context, tx pgx.Tx, g *GoldenSignals) error {
	rows, err := tx.Query(ctx, `SELECT surface_id, count(*) FROM sessions GROUP BY surface_id`)
	if err != nil {
		return fmt.Errorf("obs: query sessions by surface: %w", err)
	}
	defer rows.Close()

	g.SessionsBySurface = map[string]int{}
	for rows.Next() {
		var surface string
		var n int
		if err := rows.Scan(&surface, &n); err != nil {
			return fmt.Errorf("obs: scan sessions by surface: %w", err)
		}
		g.SessionsBySurface[surface] = n
	}
	return rows.Err()
}

func safeDiv(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
