package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
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
}

// Run is the generator (README task 2.1): hygiene -> reserve -> build prompt
// -> stream -> classify -> dispatch -> pair -> loop-or-terminate. Every
// appended event is durably committed (store.Append, inside InTenantTx)
// before it is yielded, so a caller forwarding these events (e.g. over SSE)
// never shows a client something that isn't already in the log.
func (k *Kernel) Run(ctx context.Context, st *RunState, cfg RunConfig) iter.Seq2[store.Event, error] {
	return func(yield func(store.Event, error) bool) {
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
			st.Transcript = append(st.Transcript, provider.Message{Role: "user", Text: cfg.Input})
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
		st.Transcript = append(st.Transcript, provider.Message{Role: "tool", Text: resultText(result)})
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
		st.Transcript = append(st.Transcript, provider.Message{Role: "tool", Text: resultText(result)})
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
		if err := k.updateStatus(ctx, st, store.SessionStatusRunning, nil); err != nil {
			yield(store.Event{}, err)
			return
		}
		k.runTurns(ctx, st, cfg, yield, 1)
	}
}

// runTurns is the turn loop both Run (from turn 1, after its own preamble)
// and Resume (from turn 1, after resolving the one tool_use that suspended
// the run) share: hygiene -> reserve -> build prompt -> stream -> classify
// -> dispatch -> pair -> loop-or-terminate (README task 2.1). Every
// appended event is durably committed (store.Append, inside InTenantTx)
// before it is yielded, so a caller forwarding these events (e.g. over SSE)
// never shows a client something that isn't already in the log.
func (k *Kernel) runTurns(ctx context.Context, st *RunState, cfg RunConfig, yield func(store.Event, error) bool, startTurn int) {
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}

	for turn := startTurn; ; turn++ {
		if turn > maxTurns {
			k.terminate(ctx, st, yield, TerminalMaxTurnsExceeded(maxTurns))
			return
		}

		// A ToolExecutor that also bounds hook cost per turn
		// (internal/hooks task 3.11's per-turn cap, via
		// PipelineExecutor -> tools.Pipeline) gets told a new turn has
		// started. This is an optional interface, not part of
		// ToolExecutor itself, so kernel.NotImplementedToolExecutor
		// and any future executor with nothing to reset need no
		// no-op method just to satisfy it.
		if r, ok := k.Tools.(interface{ ResetTurn() }); ok {
			r.ResetTurn()
		}

		kept, synth := Hygiene(st.History)
		st.History = kept
		for _, s := range synth {
			ev, err := k.appendToolResult(ctx, st, s.PairRef, s.ToolID, ToolResult{IsError: true, Synthetic: true, Reason: s.Reason})
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			st.Transcript = append(st.Transcript, provider.Message{Role: "tool", Text: "[synthetic error] " + s.Reason})
			if !yield(ev, nil) {
				return
			}
		}

		reservation, reserveErr := k.Budget.Reserve(ctx, cost.ReserveRequest{
			TenantID: st.TenantID, SessionID: st.SessionID, ModelID: cfg.ModelID, Purpose: cost.PurposeTurn,
		})
		bev, err := k.appendBudgetDecision(ctx, st, reservation)
		if err != nil {
			yield(store.Event{}, err)
			return
		}
		if !yield(bev, nil) {
			return
		}
		if reserveErr != nil {
			k.terminate(ctx, st, yield, TerminalCostExhausted(reservation.Decision.Reason))
			return
		}

		// Live pruning (task 7.10): a per-turn VIEW only — st.Transcript
		// itself is never reassigned here, so every other stage that still
		// reads it (a future turn's own fresh Prune call, Resume/Continue's
		// rehydration) sees the original, un-pruned content. view and
		// st.Transcript always stay the same LENGTH (Prune only rewrites a
		// candidate message's Text, never adds or removes one), which is
		// what lets the condensation step below slice st.Transcript at the
		// same index Prune's caller computed against view.
		view, prunedCount := promptctx.Prune(st.Transcript, k.PrunePolicy)
		if prunedCount > 0 {
			ev, err := k.appendEvent(ctx, st, store.EventContextPruned, store.ActorSystem, nil, nil, nil, contextPrunedPayload{PrunedCount: prunedCount})
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			if !yield(ev, nil) {
				return
			}
		}

		if ok, covered := promptctx.ShouldCondense(view, k.CondenseThresholdBytes); ok {
			summary, degraded, condenseReservation, err := k.condense(ctx, st, cfg, view[:covered])
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			cbev, err := k.appendBudgetDecision(ctx, st, condenseReservation)
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			if !yield(cbev, nil) {
				return
			}
			if degraded {
				summary = promptctx.ExtractivePass(view[:covered])
			}

			// CoveredThroughSeq: the log's own last seq at this point —
			// deliberately the whole history so far, not a precise mapping
			// from view's index back to the exact event it came from
			// (Transcript carries no parallel seq slice). A real resumable-
			// summary boundary is future work, the same kind of honest
			// scope note kernel/rehydrate.go's own doc comment already
			// makes for general resume.
			var coveredSeq int64
			if n := len(st.History); n > 0 {
				coveredSeq = st.History[n-1].Seq
			}
			cond := store.Condense(coveredSeq, summary)
			condEv, err := k.appendEvent(ctx, st, store.EventCondensation, store.ActorSystem, nil, nil, nil, condensationPayload{
				CondensationID:    cond.CondensationID.String(),
				CoveredThroughSeq: cond.CoveredThroughSeq,
				Summary:           cond.Summary,
				Degraded:          degraded,
			})
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			if !yield(condEv, nil) {
				return
			}

			// Condensation, unlike pruning, DOES rewrite the working
			// transcript going forward — that's the whole point of
			// structured compaction (distinct from the non-destructive
			// view pruning stays scoped to). st.Transcript[covered:] (not
			// view[covered:]) so the retained tail keeps its ORIGINAL text,
			// never a pruned preview/marker baked in permanently.
			newTranscript := make([]provider.Message, 0, 1+len(st.Transcript)-covered)
			newTranscript = append(newTranscript, provider.Message{Role: "assistant", Text: "[condensed summary] " + summary})
			newTranscript = append(newTranscript, st.Transcript[covered:]...)
			st.Transcript = newTranscript
			view = newTranscript
		}

		prompt, _ := promptctx.Build(cfg.System, cfg.Catalog, view)

		stream, err := k.Provider.Stream(ctx, prompt, cfg.Catalog, provider.RunContext{TenantID: st.TenantID, SessionID: st.SessionID})
		if err != nil {
			k.reconcile(ctx, st, reservation, provider.Usage{}, false)
			k.terminateFromStreamError(ctx, st, yield, err)
			return
		}

		var contentText strings.Builder
		var reasoningChunks [][]byte
		var toolUses []ToolUseRequest
		var usage provider.Usage
		var usageReported bool
		var done provider.DoneReason
		var streamErr error
		for {
			chunk, ok, nerr := stream.Next(ctx)
			if nerr != nil {
				streamErr = nerr
				break
			}
			if !ok {
				break
			}
			switch chunk.Kind {
			case provider.ChunkContent:
				contentText.WriteString(chunk.Text)
			case provider.ChunkReasoning:
				reasoningChunks = append(reasoningChunks, chunk.Opaque)
			case provider.ChunkToolUse:
				toolUses = append(toolUses, ToolUseRequest{ToolUseID: chunk.ToolUseID, ToolName: chunk.ToolName, Input: chunk.Input})
			case provider.ChunkUsage:
				usage = chunk.Usage
				usageReported = true
			case provider.ChunkDone:
				done = chunk.Done
			}
		}
		// Reconcile unconditionally, success or failure: task 4.7's
		// UNREPORTED case is a streamErr, even one arriving AFTER a
		// usage chunk was already seen — failover.go's "committed
		// after first chunk" only says a mid-stream error is never
		// retried, not that a usage figure emitted before the error is
		// still trustworthy for everything that came after it.
		// Reconcile charges the full reserved worst case instead of a
		// partial/zero usage figure from a stream that failed
		// ("an unreliable provider must not look free").
		k.reconcile(ctx, st, reservation, usage, usageReported && streamErr == nil)
		if streamErr != nil {
			k.terminateFromStreamError(ctx, st, yield, streamErr)
			return
		}

		for _, r := range reasoningChunks {
			ev, err := k.appendThought(ctx, st, cfg.ModelID, r)
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			// Reasoning is round-tripped, never shown (internal/provider's
			// doc comment) — logged, but never added to the plaintext
			// transcript a client or the next prompt sees.
			if !yield(ev, nil) {
				return
			}
		}

		if done == provider.DoneMaxOutput {
			k.terminate(ctx, st, yield, TerminalError(fmt.Errorf("provider truncated output at max_output without a natural stop")))
			return
		}

		if contentText.Len() > 0 {
			ev, err := k.appendContent(ctx, st, cfg.ModelID, contentText.String())
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			st.Transcript = append(st.Transcript, provider.Message{Role: "assistant", Text: contentText.String()})
			if !yield(ev, nil) {
				return
			}
		}

		var toolUseEvents []store.Event
		for _, tu := range toolUses {
			ev, err := k.appendToolUseEvent(ctx, st, cfg.ModelID, tu)
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			toolUseEvents = append(toolUseEvents, ev)
			st.Transcript = append(st.Transcript, provider.Message{
				Role: "assistant",
				Text: fmt.Sprintf("[tool_use %s] %s(%s)", ev.EventID, tu.ToolName, string(tu.Input)),
			})
			if !yield(ev, nil) {
				return
			}
		}

		switch Classify(toolUses, contentText.String()) {
		case ClassificationToolCalls:
			execCtx := ExecContext{TenantID: st.TenantID, SessionID: st.SessionID, AutonomyLevel: cfg.AutonomyLevel}
			for i, tu := range toolUses {
				result := k.Tools.Execute(ctx, tu, execCtx)
				ev, err := k.appendToolResult(ctx, st, toolUseEvents[i].EventID, toolUseEvents[i].ToolID, result)
				if err != nil {
					yield(store.Event{}, err)
					return
				}
				st.Transcript = append(st.Transcript, provider.Message{Role: "tool", Text: resultText(result)})
				if !yield(ev, nil) {
					return
				}

				if result.PermissionDenied {
					toolID := "unknown"
					if toolUseEvents[i].ToolID != nil {
						toolID = *toolUseEvents[i].ToolID
					}
					k.terminate(ctx, st, yield, TerminalPermissionDenied(toolID))
					return
				}
				if result.AwaitingApproval {
					k.suspendForApproval(ctx, st, yield, toolUseEvents[i].EventID, toolUseEvents[i].ToolID, tu.Input, result)
					return
				}
				if result.AwaitingDelegation {
					k.suspendForDelegation(ctx, st, yield, toolUseEvents[i].EventID, toolUseEvents[i].ToolID, result.ChildSessionID)
					return
				}

				if k.Stuck != nil {
					verdict := k.Stuck.Record(st.SessionID, tu.ToolName, tu.Input)
					if verdict.Suspected {
						sev, err := k.appendEvent(ctx, st, store.EventStuckSuspected, store.ActorSystem, nil, nil, nil, stuckSuspectedPayload{Reason: string(verdict.Reason)})
						if err != nil {
							yield(store.Event{}, err)
							return
						}
						if !yield(sev, nil) {
							return
						}
						if verdict.Terminate {
							k.terminate(ctx, st, yield, TerminalStuckTerminated(string(verdict.Reason)))
							return
						}
					}
				}
			}
			// A dispatched tool call always continues to the next turn:
			// the run isn't done until every tool_use has a paired
			// result AND the model has seen it.
		case ClassificationContent, ClassificationEmpty:
			k.terminate(ctx, st, yield, TerminalCompleted())
			return
		}
	}
}

// reconcile calls BudgetGate.Reconcile and logs (never terminates the run
// on) a failure — Reserve's own decision-persist failure already fails
// closed BEFORE any spend is incurred (internal/cost.Gate.Reserve's doc
// comment); by the time Reconcile runs, the call has already happened, so
// a reconciliation failure means the true cost may be under-accounted,
// never that the run's own turn failed. Escalating it into a hard stop
// here would let an accounting write outage kill an otherwise-healthy run.
func (k *Kernel) reconcile(ctx context.Context, st *RunState, res cost.Reservation, usage provider.Usage, reported bool) {
	if err := k.Budget.Reconcile(ctx, res, usage, reported); err != nil {
		slog.Error("kernel: cost reconciliation failed", "error", err, "session_id", st.SessionID, "reservation_id", res.ID)
	}
}

// condense runs task 7.11's metered structured-compaction call: reserve
// (cost.PurposeCompaction, the SAME Provider port every other model call
// goes through, under cfg.CondenserModelID — a cheaper model, "off the
// paying loop" per task 4.8's own wording, meaning cheaper, never
// unmetered), then stream, then reconcile — all in this one function, the
// same reserve-then-stream shape tests/contract's metering AST check
// requires of every Provider.Stream call site. Returns the reservation too,
// so the caller can still append its own EventBudgetDecision the same way
// runTurns' turn-level reserve does. degraded=true (on a reserve refusal, a
// stream/transport failure, or empty output) means the caller must apply
// promptctx.ExtractivePass instead — a condenser that can't run must never
// block or fail the turn, only degrade it.
func (k *Kernel) condense(ctx context.Context, st *RunState, cfg RunConfig, covered []provider.Message) (summary string, degraded bool, reservation cost.Reservation, err error) {
	reservation, reserveErr := k.Budget.Reserve(ctx, cost.ReserveRequest{
		TenantID: st.TenantID, SessionID: st.SessionID, ModelID: cfg.CondenserModelID, Purpose: cost.PurposeCompaction,
	})
	if reserveErr != nil {
		return "", true, reservation, nil
	}

	prompt := promptctx.CondensePrompt(covered)
	stream, serr := k.Provider.Stream(ctx, prompt, nil, provider.RunContext{TenantID: st.TenantID, SessionID: st.SessionID})
	if serr != nil {
		k.reconcile(ctx, st, reservation, provider.Usage{}, false)
		return "", true, reservation, nil
	}

	var text strings.Builder
	var usage provider.Usage
	var usageReported bool
	var streamErr error
	for {
		chunk, ok, nerr := stream.Next(ctx)
		if nerr != nil {
			streamErr = nerr
			break
		}
		if !ok {
			break
		}
		switch chunk.Kind { //nolint:exhaustive // deliberately narrow: the condenser prompt asks for plain text only — reasoning/tool_use chunks are not meaningful for a summarization call, and ChunkDone carries nothing this loop needs beyond stream.Next reporting ok=false
		case provider.ChunkContent:
			text.WriteString(chunk.Text)
		case provider.ChunkUsage:
			usage = chunk.Usage
			usageReported = true
		}
	}
	k.reconcile(ctx, st, reservation, usage, usageReported && streamErr == nil)

	if streamErr != nil || text.Len() == 0 {
		return "", true, reservation, nil
	}
	return text.String(), false, reservation, nil
}

func resultText(r ToolResult) string {
	if r.IsError {
		return "error: " + r.Reason
	}
	return string(r.Output)
}

// --- terminal helpers ---

func (k *Kernel) terminate(ctx context.Context, st *RunState, yield func(store.Event, error) bool, t Terminal) {
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

// --- append helpers: marshal -> seal -> durably append -> track in History ---

type userMessagePayload struct {
	Body string `json:"body"`
}

type contentPayload struct {
	Body string `json:"body"`
}

type thoughtPayload struct {
	Opaque []byte `json:"opaque"`
}

type toolLoadedPayload struct {
	ToolID string `json:"tool_id"`
}

type memoryLoadedPayload struct {
	Sources []string `json:"sources"`
}

type contextPrunedPayload struct {
	PrunedCount int `json:"pruned_count"`
}

// condensationPayload is what EventCondensation carries — Degraded records
// whether the no-model extractive fallback ran (task 7.2/7.11's
// degrade-capable requirement) instead of the metered condenser model.
// Mirrors internal/store.Condensation's own three fields plus this one;
// condensation_test.go's forbidden-substring scan (is_error, completed,
// succeeded, effect_class, tool_result, outcome) doesn't match "degraded",
// so this stays consistent with that invariant.
type condensationPayload struct {
	CondensationID    string `json:"condensation_id"`
	CoveredThroughSeq int64  `json:"covered_through_seq"`
	Summary           string `json:"summary"`
	Degraded          bool   `json:"degraded"`
}

type toolUsePayload struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

type toolResultPayload struct {
	Output             json.RawMessage `json:"output,omitempty"`
	IsError            bool            `json:"is_error"`
	Reason             string          `json:"reason,omitempty"`
	Synthetic          bool            `json:"synthetic,omitempty"`
	PermissionDenied   bool            `json:"permission_denied,omitempty"`
	AwaitingApproval   bool            `json:"awaiting_approval,omitempty"`
	AskKind            string          `json:"ask_kind,omitempty"`
	CanonicalDigest    []byte          `json:"canonical_digest,omitempty"`
	ApprovalMismatch   bool            `json:"approval_mismatch,omitempty"`
	EffectClass        string          `json:"effect_class,omitempty"`
	AwaitingDelegation bool            `json:"awaiting_delegation,omitempty"`
	ChildSessionID     uuid.UUID       `json:"child_session_id,omitzero"`
}

type stuckSuspectedPayload struct {
	Reason string `json:"reason"`
}

type approvalRequestedPayload struct {
	ToolID  string `json:"tool_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
	AskKind string `json:"ask_kind,omitempty"`
}

type delegationRequestedPayload struct {
	ToolID         string    `json:"tool_id,omitempty"`
	ChildSessionID uuid.UUID `json:"child_session_id"`
}

type budgetDecisionPayload struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
	BudgetID string `json:"budget_id,omitempty"`
	Reserved string `json:"reserved,omitempty"`
}

func (k *Kernel) appendEvent(ctx context.Context, st *RunState, typ store.EventType, actor store.Actor, toolID *string, pairRef *uuid.UUID, modelID *string, payload any) (store.Event, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return store.Event{}, fmt.Errorf("marshal %s payload: %w", typ, err)
	}
	sealed, digest, keyID, err := st.Seal(plaintext)
	if err != nil {
		return store.Event{}, fmt.Errorf("seal %s payload: %w", typ, err)
	}
	e := store.Event{
		EventID:       uuid.New(),
		SessionID:     st.SessionID,
		TenantID:      st.TenantID,
		SchemaVersion: store.CurrentSchemaVersion,
		Type:          typ,
		Payload:       sealed,
		PayloadDigest: digest,
		KeyID:         keyID,
		Actor:         actor,
		ToolID:        toolID,
		PairRef:       pairRef,
		ModelID:       modelID,
	}
	var out store.Event
	err = k.Store.InTenantTx(ctx, st.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var aerr error
		out, aerr = store.Append(ctx, tx, e)
		if aerr != nil {
			return aerr
		}
		if k.Receipts != nil {
			return k.Receipts(ctx, tx, out)
		}
		return nil
	})
	if err != nil {
		return store.Event{}, fmt.Errorf("append %s: %w", typ, err)
	}
	st.History = append(st.History, out)
	return out, nil
}

func (k *Kernel) appendContent(ctx context.Context, st *RunState, modelID, text string) (store.Event, error) {
	return k.appendEvent(ctx, st, store.EventContent, store.ActorModel, nil, nil, &modelID, contentPayload{Body: text})
}

func (k *Kernel) appendThought(ctx context.Context, st *RunState, modelID string, opaque []byte) (store.Event, error) {
	return k.appendEvent(ctx, st, store.EventThought, store.ActorModel, nil, nil, &modelID, thoughtPayload{Opaque: opaque})
}

func (k *Kernel) appendToolUseEvent(ctx context.Context, st *RunState, modelID string, tu ToolUseRequest) (store.Event, error) {
	toolID := tu.ToolName
	return k.appendEvent(ctx, st, store.EventToolUse, store.ActorModel, &toolID, nil, &modelID, toolUsePayload{ToolName: tu.ToolName, Input: tu.Input})
}

func (k *Kernel) appendToolResult(ctx context.Context, st *RunState, pairRef uuid.UUID, toolID *string, result ToolResult) (store.Event, error) {
	actor := store.ActorTool
	if result.Synthetic || result.PermissionDenied || result.AwaitingApproval {
		actor = store.ActorSystem // the platform produced this outcome, not the tool itself
	}
	ref := pairRef
	// toolResultPayload's fields are declared in the same names/types/order
	// as ToolResult specifically so this conversion stays valid — extend
	// both structs together.
	return k.appendEvent(ctx, st, store.EventToolResult, actor, toolID, &ref, nil, toolResultPayload(result))
}

func (k *Kernel) appendApprovalRequested(ctx context.Context, st *RunState, toolID *string, reason, askKind string) (store.Event, error) {
	tid := ""
	if toolID != nil {
		tid = *toolID
	}
	return k.appendEvent(ctx, st, store.EventApprovalRequested, store.ActorSystem, toolID, nil, nil, approvalRequestedPayload{ToolID: tid, Reason: reason, AskKind: askKind})
}

// appendBudgetDecision appends the store.EventBudgetDecision every Reserve
// resolution produces (README task 4.6 — every resolution, including
// DecisionSkip), regardless of whether the reservation was ultimately
// granted or refused. This is a separate durable write from
// internal/cost.RecordDecision's own budget_decisions row (see
// internal/cost/gate.go's doc comment on Gate for why the two aren't one
// shared transaction): internal/cost never appends to the event log
// itself — store.Append is the log's one sanctioned writer.
func (k *Kernel) appendBudgetDecision(ctx context.Context, st *RunState, res cost.Reservation) (store.Event, error) {
	payload := budgetDecisionPayload{
		Decision: string(res.Decision.Kind),
		Reason:   res.Decision.Reason,
		Reserved: res.Decision.Reserved.String(),
	}
	if res.Decision.BudgetID != nil {
		payload.BudgetID = res.Decision.BudgetID.String()
	}
	return k.appendEvent(ctx, st, store.EventBudgetDecision, store.ActorSystem, nil, nil, nil, payload)
}

func (k *Kernel) appendTerminal(ctx context.Context, st *RunState, payload terminalEventPayload) (store.Event, error) {
	return k.appendEvent(ctx, st, store.EventTerminal, store.ActorSystem, nil, nil, nil, payload)
}

func (k *Kernel) updateStatus(ctx context.Context, st *RunState, status string, terminalReason *string) error {
	return k.Store.InTenantTx(ctx, st.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.UpdateSessionStatus(ctx, tx, st.SessionID, status, terminalReason)
	})
}
