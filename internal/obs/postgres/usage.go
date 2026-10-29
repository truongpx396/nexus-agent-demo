package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
)

// ToolCallCounts counts tool_use events per tool_id, tenant-scoped
// through store.Store.InTenantTx like every other read in this package.
func (r *Reports) ToolCallCounts(ctx context.Context, tenantID uuid.UUID) (obs.ToolCallCounts, error) {
	counts := obs.ToolCallCounts{}
	err := r.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT tool_id, count(*)
			FROM events
			WHERE type = $1 AND tool_id IS NOT NULL
			GROUP BY tool_id`,
			string(eventlog.EventToolUse),
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

// ModelSpend aggregates cost_records by (model_id, currency) — the
// per-model breakdown CacheReadRate's own query (dashboard.go) already
// sums tenant-wide but never split out. Rows with a NULL model_id (a
// non-token meter, e.g. MeterSandboxSeconds — task 4.2) are excluded: this
// is a MODEL spend breakdown, not a blanket cost report.
func (r *Reports) ModelSpend(ctx context.Context, tenantID uuid.UUID) ([]obs.ModelSpend, error) {
	var out []obs.ModelSpend
	err := r.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
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
			var m obs.ModelSpend
			if err := rows.Scan(&m.ModelID, &m.Currency, &m.InputUncached, &m.InputCacheRead, &m.InputCacheWrite, &m.Output, &m.CostMinorUnits); err != nil {
				return fmt.Errorf("obs: scan model spend: %w", err)
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}
