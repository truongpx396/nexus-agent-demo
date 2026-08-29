package kernel

import "fmt"

// TerminalReason is the typed union every run ends under (README task 2.3).
// The demo ships 8 of the original design's 9 reasons — credit_exhausted is
// dropped with the billing plane (README.md §3, pattern 3) — each with a
// named producer below. Most producers are called by a phase that hasn't
// landed yet; they exist now, complete, so every switch over TerminalReason
// (starting with buildTerminalPayload below) is exhaustive from day one and
// the `exhaustive` linter (.golangci.yml) catches a future phase that adds a
// call site without also handling it in one of these switches.
type TerminalReason string

const (
	ReasonCompleted        TerminalReason = "completed"
	ReasonMaxTurnsExceeded TerminalReason = "max_turns_exceeded"
	ReasonCostExhausted    TerminalReason = "cost_exhausted"
	ReasonAborted          TerminalReason = "aborted"
	ReasonStuckTerminated  TerminalReason = "stuck_terminated"
	ReasonPermissionDenied TerminalReason = "permission_denied"
	ReasonContextOverflow  TerminalReason = "context_overflow"
	ReasonError            TerminalReason = "error"
)

// Terminal pairs a typed reason with a human-readable detail — the shape an
// EventTerminal payload is built from.
type Terminal struct {
	Reason TerminalReason
	Detail string
}

// TerminalCompleted is the loop's own normal-stop producer (Phase 2,
// kernel/loop.go): a turn classified CONTENT or EMPTY with no tool calls
// pending.
func TerminalCompleted() Terminal { return Terminal{Reason: ReasonCompleted} }

// TerminalMaxTurnsExceeded is the loop's own backstop producer (Phase 2):
// constitution Principle IV — "iteration count ... are backstops only",
// enforced here since Phase 4's cost ceiling is the primary stop signal and
// hasn't landed yet.
func TerminalMaxTurnsExceeded(maxTurns int) Terminal {
	return Terminal{Reason: ReasonMaxTurnsExceeded, Detail: fmt.Sprintf("exceeded max_turns=%d", maxTurns)}
}

// TerminalCostExhausted is internal/cost/gate.go's producer (Phase 4, README
// task 4.4) — a BudgetGate.Reserve refusal.
func TerminalCostExhausted(detail string) Terminal {
	return Terminal{Reason: ReasonCostExhausted, Detail: detail}
}

// TerminalAborted is internal/runctl's producer (Phase 6, README task 6.9) —
// the sole producer of an explicit cancel.
func TerminalAborted(detail string) Terminal {
	return Terminal{Reason: ReasonAborted, Detail: detail}
}

// TerminalStuckTerminated is internal/reliability/stuck.go's producer (Phase
// 6, README task 6.8) — a second corroborating stuck trip.
func TerminalStuckTerminated(detail string) Terminal {
	return Terminal{Reason: ReasonStuckTerminated, Detail: detail}
}

// TerminalPermissionDenied is internal/permissions/chain.go's producer
// (Phase 3, README task 3.6) — a final DENY at any layer of the 10-layer
// chain.
func TerminalPermissionDenied(toolID string) Terminal {
	return Terminal{Reason: ReasonPermissionDenied, Detail: "denied: " + toolID}
}

// TerminalContextOverflow is the loop's own producer (Phase 2), driven by
// internal/provider/failover.go's one non-retryable trigger class: a context
// window overflow is never failed over and never retried.
func TerminalContextOverflow(detail string) Terminal {
	return Terminal{Reason: ReasonContextOverflow, Detail: detail}
}

// TerminalError is the loop's own producer (Phase 2) for an unrecoverable or
// unclassified failure: a permanent failover trigger, a retryable trigger
// with no provider left to try, or a malformed stream.
func TerminalError(err error) Terminal {
	return Terminal{Reason: ReasonError, Detail: err.Error()}
}

// terminalEventPayload is the JSON shape sealed into an EventTerminal.
type terminalEventPayload struct {
	Reason TerminalReason `json:"reason"`
	Detail string         `json:"detail,omitempty"`
}

// buildTerminalPayload is the exhaustive switch over all 8 TerminalReason
// values (README task 2.3's "exhaustive linter on every switch").
func buildTerminalPayload(t Terminal) (terminalEventPayload, error) {
	switch t.Reason {
	case ReasonCompleted, ReasonMaxTurnsExceeded, ReasonCostExhausted, ReasonAborted,
		ReasonStuckTerminated, ReasonPermissionDenied, ReasonContextOverflow, ReasonError:
		return terminalEventPayload(t), nil
	default:
		return terminalEventPayload{}, fmt.Errorf("kernel: unknown terminal reason %q", t.Reason)
	}
}
