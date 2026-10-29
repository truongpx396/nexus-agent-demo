package obs

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
