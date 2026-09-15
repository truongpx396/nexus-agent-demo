package kernel

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

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
	// ReasonRefused is the run-terminal reason for a provider policy decline
	// (README task 13.7, closing F7) — added after the original 8, deliberately
	// distinct from ReasonError: a refusal is a policy signal, not a failure
	// of the harness itself.
	ReasonRefused TerminalReason = "refused"
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

// TerminalRefused is the loop's own producer (README task 13.7) for a
// provider policy decline (stop_reason "refusal") — category is the
// provider's stop_details.category when one was reported (e.g. "cyber",
// "bio"), empty if not.
func TerminalRefused(category string) Terminal {
	detail := "refused"
	if category != "" {
		detail = "refused: " + category
	}
	return Terminal{Reason: ReasonRefused, Detail: detail}
}

// terminalEventPayload is the JSON shape sealed into an EventTerminal.
type terminalEventPayload struct {
	Reason TerminalReason `json:"reason"`
	Detail string         `json:"detail,omitempty"`
}

// buildTerminalPayload is the exhaustive switch over all 9 TerminalReason
// values (README task 2.3's "exhaustive linter on every switch").
func buildTerminalPayload(t Terminal) (terminalEventPayload, error) {
	switch t.Reason {
	case ReasonCompleted, ReasonMaxTurnsExceeded, ReasonCostExhausted, ReasonAborted,
		ReasonStuckTerminated, ReasonPermissionDenied, ReasonContextOverflow, ReasonError, ReasonRefused:
		return terminalEventPayload(t), nil
	default:
		return terminalEventPayload{}, fmt.Errorf("kernel: unknown terminal reason %q", t.Reason)
	}
}

// --- terminal helpers ---

func (k *Kernel) terminate(ctx context.Context, st *RunState, yield func(store.Event, error) bool, t Terminal) {
	// Set before anything below can fail, so the root span's deferred End
	// (terminalSpanAttrs) still reflects why this call is ending even on an
	// early return here — a best-effort observability signal, never the
	// source of truth (the durable EventTerminal appended below is that).
	st.terminalReason = string(t.Reason)

	payload, err := buildTerminalPayload(t)
	if err != nil {
		yield(store.Event{}, err)
		return
	}
	ev, err := k.appendTerminal(ctx, st, payload)
	if err != nil {
		yield(store.Event{}, err)
		return
	}
	status := store.SessionStatusCompleted
	if t.Reason != ReasonCompleted {
		status = store.SessionStatusFailed
	}
	reason := string(t.Reason)
	if err := k.updateStatus(ctx, st, status, &reason); err != nil {
		yield(ev, fmt.Errorf("terminal event %s appended but session status update failed: %w", ev.EventID, err))
		return
	}
	if k.Stuck != nil {
		k.Stuck.Forget(st.SessionID) // a terminated session accumulates no further stuck-detection state
	}
	yield(ev, nil)
}

// suspendForApproval is the loop's reaction to an AwaitingApproval result
// (Phase 3's permission chain resolving ASK with no standing scope to
// satisfy it): append an EventApprovalRequested, mark the session
// suspended, and (if OnSuspend is wired) durably record an approval bound
// to toolUseEventID/digest — then stop the generator WITHOUT a terminal
// event — a suspended run is paused, not done. Resuming from here is
// Kernel.Resume (README task 5.8), driven by internal/oversight once a
// human decides; general crash/steer resume from an arbitrary point is
// still Phase 6's internal/runctl + checkpoint.
func (k *Kernel) suspendForApproval(ctx context.Context, st *RunState, yield func(store.Event, error) bool, toolUseEventID uuid.UUID, toolID *string, input json.RawMessage, result ToolResult) {
	ev, err := k.appendApprovalRequested(ctx, st, toolID, result.Reason, result.AskKind)
	if err != nil {
		yield(store.Event{}, err)
		return
	}
	if err := k.updateStatus(ctx, st, store.SessionStatusSuspended, nil); err != nil {
		yield(ev, fmt.Errorf("approval_requested event %s appended but session status update failed: %w", ev.EventID, err))
		return
	}
	if k.OnSuspend != nil {
		tid := ""
		if toolID != nil {
			tid = *toolID
		}
		err := k.Store.InTenantTx(ctx, st.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return k.OnSuspend(ctx, tx, SuspendRequest{
				TenantID: st.TenantID, SessionID: st.SessionID,
				ToolUseEventID: toolUseEventID, ApprovalEventID: ev.EventID,
				ToolID: tid, Input: input, CanonicalDigest: result.CanonicalDigest, AskKind: result.AskKind,
				EffectClass: result.EffectClass,
			})
		})
		if err != nil {
			yield(ev, fmt.Errorf("approval_requested event %s appended but OnSuspend failed: %w", ev.EventID, err))
			return
		}
	}
	yield(ev, nil)
}

// suspendForDelegation is the loop's reaction to an AwaitingDelegation result
// (README task 8.10): platform/delegate's own Call already spawned the
// child, asynchronously, before this ever runs — this appends
// EventDelegationRequested, marks the session suspended, and (if OnDelegate
// is wired) durably binds the pending delegations row internal/delegate
// already created to toolUseEventID — then stops the generator WITHOUT a
// terminal event, the same "paused, not done" shape suspendForApproval
// already uses. Resuming from here is Kernel.ResumeDelegation, driven by
// internal/delegate once the child reaches its own terminal state — never a
// human decision, which is what makes this a DIFFERENT resolution path from
// Resume even though the suspend shape is identical.
func (k *Kernel) suspendForDelegation(ctx context.Context, st *RunState, yield func(store.Event, error) bool, toolUseEventID uuid.UUID, toolID *string, childSessionID uuid.UUID) {
	tid := ""
	if toolID != nil {
		tid = *toolID
	}
	ev, err := k.appendEvent(ctx, st, store.EventDelegationRequested, store.ActorSystem, toolID, nil, nil, delegationRequestedPayload{ToolID: tid, ChildSessionID: childSessionID})
	if err != nil {
		yield(store.Event{}, err)
		return
	}
	if err := k.updateStatus(ctx, st, store.SessionStatusSuspended, nil); err != nil {
		yield(ev, fmt.Errorf("delegation_requested event %s appended but session status update failed: %w", ev.EventID, err))
		return
	}
	if k.OnDelegate != nil {
		err := k.Store.InTenantTx(ctx, st.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return k.OnDelegate(ctx, tx, DelegateSuspendRequest{
				TenantID: st.TenantID, SessionID: st.SessionID,
				ToolUseEventID: toolUseEventID, DelegationEventID: ev.EventID,
				ToolID: tid, ChildSessionID: childSessionID,
			})
		})
		if err != nil {
			yield(ev, fmt.Errorf("delegation_requested event %s appended but OnDelegate failed: %w", ev.EventID, err))
			return
		}
	}
	yield(ev, nil)
}

// terminateFromStreamError maps a Provider.Stream/Stream.Next failure onto a
// terminal reason via internal/provider/failover's typed trigger taxonomy —
// by the time an error reaches here, any failover across providers/retries
// (internal/provider/failover.Wrap) has already been exhausted, so every
// trigger class ends the run; only ContextOverflow gets its own dedicated
// reason (README task 2.9's "never fails over on context overflow" is a
// property of Wrap, not of this switch).
func (k *Kernel) terminateFromStreamError(ctx context.Context, st *RunState, yield func(store.Event, error) bool, err error) {
	switch provider.ClassifyTrigger(err) {
	case provider.TriggerContextOverflow:
		k.terminate(ctx, st, yield, TerminalContextOverflow(err.Error()))
	case provider.TriggerRetryable, provider.TriggerPermanent:
		k.terminate(ctx, st, yield, TerminalError(err))
	default:
		k.terminate(ctx, st, yield, TerminalError(err))
	}
}
