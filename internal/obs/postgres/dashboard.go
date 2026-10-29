// Package postgres is the Postgres adapter for obs.Reports: the dashboard's
// golden-signal aggregates and the /metrics per-tool and per-model breakdowns,
// each read inside the tenant's own scope (store.Store.InTenantTx — the one
// sanctioned scoping call, same as every other read in this codebase).
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Reports implements obs.Reports over Postgres.
type Reports struct {
	st *store.Store
}

var _ obs.Reports = (*Reports)(nil)

// New returns Reports that open their tenant-scoped transactions on st.
func New(st *store.Store) *Reports { return &Reports{st: st} }

// GoldenSignals queries every signal out of tenantID's own tenant-scoped rows.
func (r *Reports) GoldenSignals(ctx context.Context, tenantID uuid.UUID, staleClaimAfter time.Duration) (obs.GoldenSignals, error) {
	var g obs.GoldenSignals
	err := r.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
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

func queryCompletionByReason(ctx context.Context, tx pgx.Tx, g *obs.GoldenSignals) error {
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

func queryCostCeilingBreachRate(ctx context.Context, tx pgx.Tx, g *obs.GoldenSignals) error {
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

func queryCacheReadRate(ctx context.Context, tx pgx.Tx, g *obs.GoldenSignals) error {
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

func queryApprovalSignals(ctx context.Context, tx pgx.Tx, g *obs.GoldenSignals) error {
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
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = $1`, string(eventlog.EventApprovalMismatch)).Scan(&mismatches); err != nil {
		return fmt.Errorf("obs: count approval_mismatch events: %w", err)
	}
	g.ApprovalMismatchRate = safeDiv(mismatches, decided)
	return nil
}

func queryUnresolvedClaims(ctx context.Context, tx pgx.Tx, g *obs.GoldenSignals, staleAfter time.Duration) error {
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

func queryTaintTransitionRate(ctx context.Context, tx pgx.Tx, g *obs.GoldenSignals) error {
	var sessionsWithTransition int
	err := tx.QueryRow(ctx,
		`SELECT count(DISTINCT session_id) FROM events WHERE type = $1`,
		string(eventlog.EventTaintTransition),
	).Scan(&sessionsWithTransition)
	if err != nil {
		return fmt.Errorf("obs: count taint_transition sessions: %w", err)
	}
	g.TaintTransitionRate = safeDiv(sessionsWithTransition, g.TotalTerminalSessions)
	return nil
}

func querySessionsBySurface(ctx context.Context, tx pgx.Tx, g *obs.GoldenSignals) error {
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
