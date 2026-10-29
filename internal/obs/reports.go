package obs

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Reports is the read side of the dashboard and the /metrics scrape: the
// aggregates a deployment watches without ever reading conversation content.
// Each method answers for one tenant from that tenant's own rows only. The
// Postgres adapter is internal/obs/postgres; it reads structure (a status
// column, a decision kind, a meter quantity, a timestamp), never a payload —
// the same content-free discipline Filter enforces for spans.
type Reports interface {
	// GoldenSignals computes every signal in GoldenSignals except the two
	// that do not come from storage (TelemetryAttrDropRate, HeldOutGap),
	// which ComputeGoldenSignals folds in.
	GoldenSignals(ctx context.Context, tenantID uuid.UUID, staleClaimAfter time.Duration) (GoldenSignals, error)

	// ToolCallCounts counts tool_use events per tool_id.
	ToolCallCounts(ctx context.Context, tenantID uuid.UUID) (ToolCallCounts, error)

	// ModelSpend aggregates cost_records by (model_id, currency).
	ModelSpend(ctx context.Context, tenantID uuid.UUID) ([]ModelSpend, error)
}

// ComputeGoldenSignals reads tenantID's signals from r and adds the two that
// are not stored: drops and heldOutGap may be nil; a nil drops leaves
// TelemetryAttrDropRate at 0 rather than erroring, since not every caller
// wires an exporter through a DropTracker.
func ComputeGoldenSignals(ctx context.Context, r Reports, tenantID uuid.UUID, staleClaimAfter time.Duration, drops *DropTracker, heldOutGap *float64) (GoldenSignals, error) {
	g, err := r.GoldenSignals(ctx, tenantID, staleClaimAfter)
	g.HeldOutGap = heldOutGap
	if drops != nil {
		g.TelemetryAttrDropRate = drops.Rate()
	}
	return g, err
}
