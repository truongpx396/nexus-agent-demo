package obs

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
