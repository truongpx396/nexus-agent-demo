package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/promptctx"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/reliability"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// defaultMaxTurns is the backstop constitution Principle IV calls
// "iteration count ... are backstops only" — cost (Phase 4) is the primary
// stop signal; this exists so a Phase 2 run can't loop forever with no cost
// gate installed yet.
const defaultMaxTurns = 25

// Kernel is THE loop (docs/constitution.md, Principle I). Provider is
// assumed already wrapped by internal/provider/failover.Wrap if failover
// across replicas is wanted — the loop itself only ever calls Stream once
// per turn and lets that wrapping (or its absence) decide what happens on
// failure.
type Kernel struct {
	Provider provider.Provider
	Tools    ToolExecutor
	Budget   BudgetGate
	Store    *store.Store

	// Receipts extends the hash-chained audit receipt for every event this
	// Kernel appends (internal/audit, Phase 5, README task 5.2). Nil is
	// valid — every pre-Phase-5 test constructs a Kernel without one — and
	// simply means no receipt is written for that call.
	Receipts ReceiptFunc

	// OnSuspend durably records an approval for the tool_use a run just
	// suspended on (internal/oversight.Approvals.Create, Phase 5, README
	// task 5.6). Nil is valid — every pre-Phase-5 test — and simply means
	// nothing beyond EventApprovalRequested itself is recorded.
	OnSuspend OnSuspend

	// OnDelegate durably binds a delegation to the tool_use it gates, right
	// after suspendForDelegation appends EventDelegationRequested
	// (internal/delegate.Delegations.Bind, Phase 8, README task 8.10). Nil
	// is valid — every pre-Phase-8 test — and simply means nothing beyond
	// EventDelegationRequested itself is recorded.
	OnDelegate OnDelegate

	// Stuck is task 6.8's per-turn observer (internal/reliability/stuck.go,
	// Phase 6): folds each dispatched tool_use into that session's own
	// Tracker and, on a SECOND corroborating trip, is what makes
	// TerminalStuckTerminated fire (that function's own doc comment names
	// this call site). Nil is valid — every pre-Phase-6 test — and simply
	// means stuck detection never runs, exactly like a nil Receipts/
	// OnSuspend already means "that phase's control isn't wired."
	Stuck *reliability.Registry

	// PrunePolicy is README task 7.10's live pruning, applied to a per-turn
	// VIEW of st.Transcript only — st.Transcript itself, and every durably
	// logged event, are never mutated by it. The zero value
	// (promptctx.PrunePolicy{}) prunes nothing, the pre-Phase-7 behavior
	// every earlier test still gets.
	PrunePolicy promptctx.PrunePolicy

	// CondenseThresholdBytes is task 7.11's structured-compaction trigger:
	// once the (post-prune) transcript's total byte length exceeds this,
	// runTurns replaces its covered prefix with one metered-model summary
	// (or, if internal/cost.BudgetGate.Reserve refuses, a local
	// promptctx.ExtractivePass — "degrade-capable," never skipped). <=0
	// disables condensation entirely, the pre-Phase-7 default.
	CondenseThresholdBytes int

	// Tracer, if set, gives an agent's run genuine per-step observability
	// (docs/local-llm.md) — one root span per Run/Resume/ResumeDelegation/
	// Continue call, a generation span per model call, a tool span per
	// dispatched tool_use, all through k.startSpan below. Nil is valid, the
	// same "this control isn't wired" convention Receipts/OnSuspend/Stuck
	// already use — every pre-this-change test constructing a bare
	// Kernel{...} is unaffected.
	Tracer obs.Tracer

	// TraceContent, if true, additionally attaches each generation/tool
	// span's actual input/output payload (obs.Span.SetContent) — a
	// deliberate, explicit content-egress decision (NEXUS_TRACE_CONTENT,
	// docs/local-llm.md), analogous to but distinct from the audited,
	// expiring Content Access Grant that is otherwise the only sanctioned
	// way content leaves the event log (constitution Principle VI). False
	// (the default, and every pre-this-change test's zero value) means
	// every span stays exactly as content-free as Tracer's own doc comment
	// above already promises — this field does not change that promise, it
	// is what an operator has to explicitly set to step outside it.
	TraceContent bool
}

// startSpan opens a span through k.Tracer if one is wired, or a no-op if
// not — every instrumentation call site below goes through this so none of
// them need their own nil check (mirrors how k.reconcile/k.terminate already
// centralize a repeated concern instead of leaving it to each call site).
func (k *Kernel) startSpan(ctx context.Context, name string, kind obs.ObservationType, attrs obs.Attrs) (context.Context, obs.Span) {
	if k.Tracer == nil {
		return ctx, noopSpan{}
	}
	return k.Tracer.StartSpan(ctx, name, kind, attrs)
}

type noopSpan struct{}

func (noopSpan) End(obs.Attrs)             {}
func (noopSpan) SetContent(string, string) {}

// terminalSpanAttrs is what each of Run/Resume/ResumeDelegation/Continue's
// deferred root-span End passes: st.terminalReason if terminate() set one
// this call, or no attribute at all if this call ended by suspending
// instead (an approval or delegation is still pending) — never a
// misleading empty-string terminal_reason.
func terminalSpanAttrs(st *RunState) obs.Attrs {
	if st.terminalReason == "" {
		return obs.Attrs{}
	}
	return obs.Attrs{"terminal_reason": st.terminalReason}
}

// generationOutput is a model turn's Span.SetContent output payload — text
// plus any tool calls it requested, since a turn that only calls tools may
// have no text at all.
type generationOutput struct {
	Text     string           `json:"text,omitempty"`
	ToolUses []ToolUseRequest `json:"tool_uses,omitempty"`
}

// marshalContent is Span.SetContent's own marshaling helper: best-effort,
// never called unless k.TraceContent is true (every call site above already
// checks), and an encode failure just means no content for this one span —
// never a reason to fail the turn a content-free run would otherwise
// complete normally.
func marshalContent(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// RunState is the mutable state one Run call owns: TenantID/SessionID
// identify the session; Seal is how a plaintext payload becomes what
// store.Append persists; History is the durable, sealed event log (used by
// Hygiene, which only ever reads structural fields — never decrypted here);
// Transcript is the plaintext, in-memory projection promptctx.Build works
// from, built up as the kernel itself produces or receives each message so
// nothing needs decrypting mid-run. Both start empty for a fresh run —
// resuming a History pre-hydrated from a prior run is Phase 6's concern.
type RunState struct {
	TenantID   uuid.UUID
	SessionID  uuid.UUID
	Seal       SealFunc
	History    []store.Event
	Transcript []provider.Message

	// ToolUseIDs maps a tool_use event's internal EventID to the
	// PROVIDER-assigned tool_use_id Rehydrate reconstructed it under
	// (kernel/rehydrate.go's second return value) — how Resume/
	// ResumeDelegation below pair a resolved result back to the right
	// wire-level tool_use block without trusting a caller-supplied claim.
	// nil for a fresh Run/Continue, which never needs it: a live turn
	// already has the provider's tool_use_id in hand via ToolUseRequest
	// itself.
	ToolUseIDs map[uuid.UUID]string

	// terminalReason carries the reason terminate() records (below) out to
	// the deferred root span each of Run/Resume/ResumeDelegation/Continue
	// opens — set once, in terminate(), read once, by that defer. Empty
	// when a call ends by suspending rather than terminating (Resume is
	// still pending, a delegation is still pending) — the root span's own
	// End simply omits the attribute rather than record something untrue.
	terminalReason string
}

// Run is the generator (README task 2.1): hygiene -> reserve -> build prompt
// -> stream -> classify -> dispatch -> pair -> loop-or-terminate. Every
// appended event is durably committed (store.Append, inside InTenantTx)
// before it is yielded, so a caller forwarding these events (e.g. over SSE)
// never shows a client something that isn't already in the log.
func (k *Kernel) Run(ctx context.Context, st *RunState, cfg RunConfig) iter.Seq2[store.Event, error] {
	return func(yield func(store.Event, error) bool) {
		ctx, rootSpan := k.startSpan(ctx, "kernel.run", obs.ObservationAgent, obs.Attrs{
			"session.id": st.SessionID.String(), "tenant.id": st.TenantID.String(),
		})
		defer func() { rootSpan.End(terminalSpanAttrs(st)) }()

		if err := k.updateStatus(ctx, st, store.SessionStatusRunning, nil); err != nil {
			yield(store.Event{}, err)
			return
		}

		for _, toolID := range cfg.LoadedTools {
			ev, err := k.appendEvent(ctx, st, store.EventToolLoaded, store.ActorSystem, &toolID, nil, nil, toolLoadedPayload{ToolID: toolID})
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			// Deliberately NOT added to st.Transcript: the model already
			// sees the resident catalog via cfg.Catalog on every
			// Provider.Stream call (promptctx's two-zone builder), so this
			// is an audit record of what was pinned, not something the
			// model needs to read as a message.
			if !yield(ev, nil) {
				return
			}
		}

		if len(cfg.MemorySources) > 0 {
			ev, err := k.appendEvent(ctx, st, store.EventMemoryLoaded, store.ActorSystem, nil, nil, nil, memoryLoadedPayload{Sources: cfg.MemorySources})
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			// Deliberately NOT added to st.Transcript, same reasoning as
			// EventToolLoaded above: the memory text is already folded into
			// cfg.System (memory.Snapshot's caller does this before Run is
			// ever called — README task 7.1's "injected at session start"),
			// so this is the audit record of what was pinned, not something
			// the model needs to read as a message.
			if !yield(ev, nil) {
				return
			}
		}

		if cfg.Input != "" {
			ev, err := k.appendEvent(ctx, st, store.EventUserMessage, store.ActorUser, nil, nil, nil, userMessagePayload{Body: cfg.Input})
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			st.Transcript = append(st.Transcript, provider.TextMessage("user", cfg.Input))
			if !yield(ev, nil) {
				return
			}
		}

		k.runTurns(ctx, st, cfg, yield, 1)
	}
}

// Resume continues a session a run suspended on an approval (kernel/
// loop.go's suspendForApproval), acting on internal/oversight's resolution
// for the ONE pending tool_use that suspended it (README task 5.8). st must
// already be rehydrated (Rehydrate, kernel/rehydrate.go) — History and
// Transcript populated from the session's stored event log up to and
// including that tool_use — before this is called; Resume itself neither
// replays nor decrypts anything.
//
// This is deliberately scoped to the approval-suspend case only, not
// general crash/steer resume from an arbitrary point — Phase 6's
// internal/runctl + the real Checkpoint artifact (README task 6.3) still
// own that; this is the same kind of honest interim WorkspaceRoot already
// is for the sandbox.
func (k *Kernel) Resume(ctx context.Context, st *RunState, cfg RunConfig, res PendingResolution) iter.Seq2[store.Event, error] {
	return func(yield func(store.Event, error) bool) {
		ctx, rootSpan := k.startSpan(ctx, "kernel.resume", obs.ObservationAgent, obs.Attrs{
			"session.id": st.SessionID.String(), "tenant.id": st.TenantID.String(),
		})
		defer func() { rootSpan.End(terminalSpanAttrs(st)) }()

		if err := k.updateStatus(ctx, st, store.SessionStatusRunning, nil); err != nil {
			yield(store.Event{}, err)
			return
		}

		input := res.Input
		if res.Decision == ApprovalDecisionGrantedModified {
			input = res.ModifiedInput
		}

		var result ToolResult
		switch res.Decision {
		case ApprovalDecisionGranted, ApprovalDecisionGrantedModified:
			executor, ok := k.Tools.(ApprovedExecutor)
			if !ok {
				yield(store.Event{}, fmt.Errorf("kernel: Resume called but the configured ToolExecutor does not implement ApprovedExecutor"))
				return
			}
			execCtx := ExecContext{TenantID: st.TenantID, SessionID: st.SessionID, AutonomyLevel: cfg.AutonomyLevel}
			result = executor.ExecuteApproved(ctx, ToolUseRequest{ToolName: res.ToolID, Input: input}, res.ApprovedDigest, execCtx)
		case ApprovalDecisionDenied:
			result = ToolResult{IsError: true, Synthetic: true, PermissionDenied: true, Reason: "approval_denied: " + res.Reason}
		case ApprovalDecisionInvalidated:
			result = ToolResult{IsError: true, Synthetic: true, PermissionDenied: true, Reason: "approval_invalidated: " + res.Reason}
		}

		toolID := res.ToolID
		ev, err := k.appendToolResult(ctx, st, res.ToolUseEventID, &toolID, result)
		if err != nil {
			yield(store.Event{}, err)
			return
		}
		st.Transcript = append(st.Transcript, provider.ToolResultMessage(st.ToolUseIDs[res.ToolUseEventID], resultText(result), result.IsError))
		if !yield(ev, nil) {
			return
		}

		// A denial, an invalidation, and an approval_mismatch are all the
		// same severity class as a chain-level DENY (kernel.ToolResult's
		// own doc comment on PermissionDenied): fatal to the run, not just
		// to this one call — never silently continue past a refused
		// execution. AwaitingApproval is checked only defensively:
		// ExecuteApproved never resolves Ask by construction (it skips the
		// permission chain entirely), so this would only fire on an
		// ApprovedExecutor implementation bug.
		if result.PermissionDenied || result.ApprovalMismatch {
			k.terminate(ctx, st, yield, TerminalPermissionDenied(res.ToolID))
			return
		}
		if result.AwaitingApproval {
			yield(store.Event{}, fmt.Errorf("kernel: Resume's ApprovedExecutor unexpectedly resolved AwaitingApproval for %s", res.ToolID))
			return
		}

		k.runTurns(ctx, st, cfg, yield, 1)
	}
}

// ResumeDelegation continues a session a run suspended on a delegation
// (kernel/loop.go's suspendForDelegation), acting on internal/delegate's
// resolution for the ONE pending tool_use that suspended it (README task
// 8.10). st must already be rehydrated (History/Transcript populated up to
// and including that tool_use) before this is called, exactly like Resume's
// own contract — ResumeDelegation itself neither replays nor decrypts
// anything, and never re-runs the delegate tool's own Call (unlike
// ExecuteApproved, which re-runs an approval-gated Tool.Call on purpose):
// the child already ran; this only delivers its outcome as the paired
// tool_result.
func (k *Kernel) ResumeDelegation(ctx context.Context, st *RunState, cfg RunConfig, res DelegationResolution) iter.Seq2[store.Event, error] {
	return func(yield func(store.Event, error) bool) {
		ctx, rootSpan := k.startSpan(ctx, "kernel.resume_delegation", obs.ObservationAgent, obs.Attrs{
			"session.id": st.SessionID.String(), "tenant.id": st.TenantID.String(),
		})
		defer func() { rootSpan.End(terminalSpanAttrs(st)) }()

		if err := k.updateStatus(ctx, st, store.SessionStatusRunning, nil); err != nil {
			yield(store.Event{}, err)
			return
		}

		var result ToolResult
		switch res.Outcome {
		case DelegationReturned:
			result = ToolResult{Output: res.Result}
		case DelegationReaped:
			result = ToolResult{IsError: true, Synthetic: true, Reason: "delegation_reaped: " + res.Reason}
		case DelegationBoundExceeded:
			result = ToolResult{IsError: true, Synthetic: true, Reason: "bound_exceeded: " + res.Reason}
		}

		toolID := res.ToolID
		ev, err := k.appendToolResult(ctx, st, res.ToolUseEventID, &toolID, result)
		if err != nil {
			yield(store.Event{}, err)
			return
		}
		st.Transcript = append(st.Transcript, provider.ToolResultMessage(st.ToolUseIDs[res.ToolUseEventID], resultText(result), result.IsError))
		if !yield(ev, nil) {
			return
		}

		k.runTurns(ctx, st, cfg, yield, 1)
	}
}

// Continue re-enters the turn loop for a session that already has durable
// history — general crash/steer resume from an arbitrary point
// (internal/runctl.Resume, README task 6.9), as distinct from Run (a fresh
// session, appends an opening EventUserMessage first) and Resume above
// (scoped narrowly to resolving the ONE tool_use an approval suspended a
// run on). st must already be rehydrated (History/Transcript populated via
// kernel.Rehydrate, exactly like Resume's own loadRunState convention) —
// Continue itself does no replay or decrypt of its own.
//
// The turn loop's own first step, Hygiene, is what makes this safe after a
// crash: any tool_use a prior process left unpaired (killed mid-Call) gets
// a synthesized "interrupted_before_execution" result before the next model
// call, never a silent re-execution — Hygiene's own doc comment already
// names Phase 6's queue+checkpoint as the trigger this method is.
func (k *Kernel) Continue(ctx context.Context, st *RunState, cfg RunConfig) iter.Seq2[store.Event, error] {
	return func(yield func(store.Event, error) bool) {
		ctx, rootSpan := k.startSpan(ctx, "kernel.continue", obs.ObservationAgent, obs.Attrs{
			"session.id": st.SessionID.String(), "tenant.id": st.TenantID.String(),
		})
		defer func() { rootSpan.End(terminalSpanAttrs(st)) }()

		if err := k.updateStatus(ctx, st, store.SessionStatusRunning, nil); err != nil {
			yield(store.Event{}, err)
			return
		}
		k.runTurns(ctx, st, cfg, yield, 1)
	}
}

// ResumeConversation continues a session a conversational run
// (cfg.Conversational, kernel/types.go's own doc comment) paused on via
// suspendForUserInput — the human's next message, arriving out of band
// (internal/runctl.Control.ResumeConversation) rather than as part of the
// same original request. Unlike Continue (re-enters the turn loop as-is,
// for crash/steer resume from an arbitrary point) this appends a fresh
// EventUserMessage first, exactly like Run's own opening message — the
// same "append the human's turn, then run the loop" shape, just against a
// session that already has a full Transcript instead of an empty one. st
// must already be rehydrated (History/Transcript populated via
// kernel.Rehydrate), the same contract Resume/Continue already have;
// ResumeConversation itself does no replay or decrypt of its own.
func (k *Kernel) ResumeConversation(ctx context.Context, st *RunState, cfg RunConfig, input string) iter.Seq2[store.Event, error] {
	return func(yield func(store.Event, error) bool) {
		ctx, rootSpan := k.startSpan(ctx, "kernel.resume_conversation", obs.ObservationAgent, obs.Attrs{
			"session.id": st.SessionID.String(), "tenant.id": st.TenantID.String(),
		})
		defer func() { rootSpan.End(terminalSpanAttrs(st)) }()

		if err := k.updateStatus(ctx, st, store.SessionStatusRunning, nil); err != nil {
			yield(store.Event{}, err)
			return
		}

		ev, err := k.appendEvent(ctx, st, store.EventUserMessage, store.ActorUser, nil, nil, nil, userMessagePayload{Body: input})
		if err != nil {
			yield(store.Event{}, err)
			return
		}
		st.Transcript = append(st.Transcript, provider.TextMessage("user", input))
		if !yield(ev, nil) {
			return
		}

		k.runTurns(ctx, st, cfg, yield, 1)
	}
}
