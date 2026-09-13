package obs

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// ToolCallCounts is tool_id -> the number of tool_use events recorded
// against it (docs/build-phases.md's Phase 17, task 17.9 — the per-tool
// breakdown the Golden Signals dashboard's own aggregate rates don't show).
// Sourced from events.tool_id — a plain, structural column
// (migrations/0003_events.sql), never the encrypted payload — the same
// content-free discipline ComputeGoldenSignals already follows. This is
// deliberately NOT read from internal/cost.MeterToolInvocations: that meter
// is registered but never emitted (cost/meter.go's own doc comment), so the
// event log is the only place a tool call is actually durable today.
type ToolCallCounts map[string]int64

// ComputeToolCallCounts counts tool_use events per tool_id, tenant-scoped
// through store.Store.InTenantTx like every other read in this package.
func ComputeToolCallCounts(ctx context.Context, st *store.Store, tenantID uuid.UUID) (ToolCallCounts, error) {
	counts := ToolCallCounts{}
	err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT tool_id, count(*)
			FROM events
			WHERE type = $1 AND tool_id IS NOT NULL
			GROUP BY tool_id`,
			string(store.EventToolUse),
		)
		if err != nil {
			return fmt.Errorf("obs: query tool call counts: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var toolID string
			var n int64
			if err := rows.Scan(&toolID, &n); err != nil {
				return fmt.Errorf("obs: scan tool call count: %w", err)
			}
			counts[toolID] = n
		}
		return rows.Err()
	})
	return counts, err
}

// ModelSpend is one routed model's aggregated cost_records rows (task
// 17.9): every token class internal/cost.Reconcile prices separately
// (#37's own per-class discipline), plus the reconciled cost. CostMinorUnits
// stays the exact integer internal/cost/money.go's Money type computed —
// obs never touches float64 itself; that conversion happens exactly once,
// at the Prometheus text-exposition boundary in cmd/nexusd/main.go, the
// same "rounding once at the asserted boundary" rule task 4.1 applies to
// every other cost computation.
type ModelSpend struct {
	ModelID         string
	Currency        string
	InputUncached   int64
	InputCacheRead  int64
	InputCacheWrite int64
	Output          int64
	CostMinorUnits  int64
}

// ComputeModelSpend aggregates cost_records by (model_id, currency) — the
// per-model breakdown CacheReadRate's own query (dashboard.go) already
// sums tenant-wide but never split out. Rows with a NULL model_id (a
// non-token meter, e.g. MeterSandboxSeconds — task 4.2) are excluded: this
// is a MODEL spend breakdown, not a blanket cost report.
func ComputeModelSpend(ctx context.Context, st *store.Store, tenantID uuid.UUID) ([]ModelSpend, error) {
	var out []ModelSpend
	err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT
				model_id,
				currency,
				coalesce(sum(quantity) FILTER (WHERE meter = $1), 0),
				coalesce(sum(quantity) FILTER (WHERE meter = $2), 0),
				coalesce(sum(quantity) FILTER (WHERE meter = $3), 0),
				coalesce(sum(quantity) FILTER (WHERE meter = $4), 0),
				coalesce(sum(minor_units), 0)
			FROM cost_records
			WHERE model_id IS NOT NULL
			GROUP BY model_id, currency`,
			string(cost.MeterInputUncached), string(cost.MeterInputCacheRead),
			string(cost.MeterInputCacheWrite), string(cost.MeterOutput),
		)
		if err != nil {
			return fmt.Errorf("obs: query model spend: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var m ModelSpend
			if err := rows.Scan(&m.ModelID, &m.Currency, &m.InputUncached, &m.InputCacheRead, &m.InputCacheWrite, &m.Output, &m.CostMinorUnits); err != nil {
				return fmt.Errorf("obs: scan model spend: %w", err)
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}
