package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/promptctx"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

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
		var synthBlocks []provider.ContentBlock
		for _, s := range synth {
			ev, err := k.appendToolResult(ctx, st, s.PairRef, s.ToolID, ToolResult{IsError: true, Synthetic: true, Reason: s.Reason})
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			synthBlocks = append(synthBlocks, provider.ContentBlock{
				Kind: provider.BlockToolResult, ToolUseID: st.ToolUseIDs[s.PairRef],
				Text: "[synthetic error] " + s.Reason, IsError: true,
			})
			if !yield(ev, nil) {
				return
			}
		}
		if len(synthBlocks) > 0 {
			st.Transcript = append(st.Transcript, provider.Message{Role: "tool", Blocks: synthBlocks})
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
			newTranscript = append(newTranscript, provider.TextMessage("assistant", "[condensed summary] "+summary))
			newTranscript = append(newTranscript, st.Transcript[covered:]...)
			st.Transcript = newTranscript
			view = newTranscript
		}

		prompt, _ := promptctx.Build(cfg.System, cfg.Catalog, view)

		// turnCtx carries this turn's own generation span as parent — reused
		// below, in the same iteration, as the parent for that turn's tool
		// spans too (a fresh child context per turn, never leaked across
		// iterations), so a Langfuse trace reads as root -> this turn's
		// model call -> the tool calls it made, not a flat list of siblings.
		turnCtx, genSpan := k.startSpan(ctx, "model.call", obs.ObservationGeneration, obs.Attrs{"model.id": cfg.ModelID})

		stream, err := k.Provider.Stream(turnCtx, prompt, cfg.Catalog, provider.RunContext{TenantID: st.TenantID, SessionID: st.SessionID})
		if err != nil {
			genSpan.End(obs.Attrs{"outcome": "error"})
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
		var refusalCategory string
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
				if k.OnChunk != nil {
					k.OnChunk(ChunkEvent{TenantID: st.TenantID, SessionID: st.SessionID, Chunk: chunk})
				}
			case provider.ChunkReasoning:
				reasoningChunks = append(reasoningChunks, chunk.Opaque)
				if k.OnChunk != nil {
					// Opaque is deliberately never forwarded — a caller only
					// ever learns THAT reasoning is in flight, never its
					// content, enforced here rather than trusted to whatever
					// OnChunk does with it (the same fail-closed posture
					// Taint's own doc comment names for a different field).
					k.OnChunk(ChunkEvent{TenantID: st.TenantID, SessionID: st.SessionID, Chunk: provider.Chunk{Kind: provider.ChunkReasoning}})
				}
			case provider.ChunkToolUse:
				toolUses = append(toolUses, ToolUseRequest{ToolUseID: chunk.ToolUseID, ToolName: chunk.ToolName, Input: chunk.Input})
				if k.OnChunk != nil {
					k.OnChunk(ChunkEvent{TenantID: st.TenantID, SessionID: st.SessionID, Chunk: chunk})
				}
			case provider.ChunkUsage:
				usage = chunk.Usage
				usageReported = true
			case provider.ChunkDone:
				done = chunk.Done
				refusalCategory = chunk.RefusalCategory
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
		outcome := string(done)
		if streamErr != nil {
			outcome = "error"
		}
		if k.TraceContent {
			genSpan.SetContent(marshalContent(prompt), marshalContent(generationOutput{Text: contentText.String(), ToolUses: toolUses}))
		}
		genSpan.End(obs.Attrs{
			"model.id":                cfg.ModelID,
			"usage.input_uncached":    strconv.Itoa(usage.InputUncached),
			"usage.input_cache_read":  strconv.Itoa(usage.InputCacheRead),
			"usage.input_cache_write": strconv.Itoa(usage.InputCacheWrite),
			"usage.output":            strconv.Itoa(usage.OutputTokens),
			"outcome":                 outcome,
		})
		k.reconcile(ctx, st, reservation, usage, usageReported && streamErr == nil)
		if streamErr != nil {
			k.terminateFromStreamError(ctx, st, yield, streamErr)
			return
		}

		// One model turn's thinking/text/tool_use blocks are consolidated
		// into a SINGLE assistant Message (README task 13.4, closing F5) —
		// never split across several transcript entries, which is exactly
		// what used to erase tool_use/tool_result pairing at the wire and
		// (per the Anthropic API's own documented guidance) silently trains
		// a model to stop making parallel tool calls. Durable events are
		// still appended one row per thought/content/tool_use, unchanged —
		// only this in-memory Transcript view is consolidated.
		var assistantBlocks []provider.ContentBlock
		for _, r := range reasoningChunks {
			ev, err := k.appendThought(ctx, st, cfg.ModelID, r)
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			// Reasoning is never shown to a client (internal/provider's doc
			// comment), but IS folded into the assistant turn's own
			// Transcript entry, ahead of any text/tool_use block — required
			// by the API when a thinking block precedes a tool_use in the
			// same turn (task 13.7). A pre-13.7/non-thinking provider's
			// Opaque bytes simply fail this decode, adding no block.
			var ro reasoningOpaque
			if err := json.Unmarshal(r, &ro); err == nil && (ro.Thinking != "" || ro.Signature != "") {
				assistantBlocks = append(assistantBlocks, provider.ContentBlock{Kind: provider.BlockThinking, Text: ro.Thinking, Signature: ro.Signature})
			}
			if !yield(ev, nil) {
				return
			}
		}

		if done == provider.DoneMaxOutput {
			k.terminate(ctx, st, yield, TerminalError(fmt.Errorf("provider truncated output at max_output without a natural stop")))
			return
		}
		if done == provider.DoneRefusal {
			k.terminate(ctx, st, yield, TerminalRefused(refusalCategory))
			return
		}

		if contentText.Len() > 0 {
			ev, err := k.appendContent(ctx, st, cfg.ModelID, contentText.String())
			if err != nil {
				yield(store.Event{}, err)
				return
			}
			assistantBlocks = append(assistantBlocks, provider.ContentBlock{Kind: provider.BlockText, Text: contentText.String()})
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
			assistantBlocks = append(assistantBlocks, provider.ContentBlock{Kind: provider.BlockToolUse, ToolUseID: tu.ToolUseID, ToolName: tu.ToolName, Input: tu.Input})
			if !yield(ev, nil) {
				return
			}
		}

		if len(assistantBlocks) > 0 {
			st.Transcript = append(st.Transcript, provider.Message{Role: "assistant", Blocks: assistantBlocks})
		}

		switch Classify(toolUses, contentText.String()) {
		case ClassificationToolCalls:
			execCtx := ExecContext{TenantID: st.TenantID, SessionID: st.SessionID, AutonomyLevel: cfg.AutonomyLevel}
			// Every tool_result this turn produces before any suspend/
			// terminate below is grouped into ONE "tool" role Message,
			// appended once after the loop — the mirror image of
			// assistantBlocks above, and for the same reason (README task
			// 13.4). An early return (permission denied, awaiting
			// approval/delegation, stuck) skips this append harmlessly:
			// Resume/Continue always rebuild Transcript fresh from durable
			// history via Rehydrate, never from this in-memory value.
			var resultBlocks []provider.ContentBlock
			for i, tu := range toolUses {
				// Parented on turnCtx (this turn's own generation span,
				// started above), not the outer ctx — so this trace's tool
				// nodes nest under the model turn that requested them,
				// rather than sitting as flat siblings of every other turn's
				// own tool calls.
				toolCtx, toolSpan := k.startSpan(turnCtx, "tool.call", obs.ObservationTool, obs.Attrs{"tool.id": tu.ToolName})
				result := k.Tools.Execute(toolCtx, tu, execCtx)
				outcome := "ok"
				if result.IsError {
					outcome = "error"
				}
				if k.TraceContent {
					toolSpan.SetContent(string(tu.Input), resultText(result))
				}
				toolSpan.End(obs.Attrs{"outcome": outcome})

				ev, err := k.appendToolResult(ctx, st, toolUseEvents[i].EventID, toolUseEvents[i].ToolID, result)
				if err != nil {
					yield(store.Event{}, err)
					return
				}
				resultBlocks = append(resultBlocks, provider.ContentBlock{Kind: provider.BlockToolResult, ToolUseID: tu.ToolUseID, Text: resultText(result), IsError: result.IsError})
				if !yield(ev, nil) {
					return
				}

				// The durable half of Rule-of-Two taint state (README
				// pattern 21's own "session taint state as a projection" --
				// kernel.RehydrateTaint is the read half): appended
				// regardless of what happens next (permission denied,
				// awaiting approval/delegation, stuck) -- the leg was
				// engaged for real the moment the permission chain resolved
				// it (tools.ExecuteResult's own doc comment), whether or
				// not this call's own outcome goes on to suspend or
				// terminate the run.
				if result.TaintChanged {
					tev, err := k.appendEvent(ctx, st, store.EventTaintTransition, store.ActorSystem, nil, nil, nil, taintTransitionPayload{Engaged: result.TaintEngaged})
					if err != nil {
						yield(store.Event{}, err)
						return
					}
					if !yield(tev, nil) {
						return
					}
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
			if len(resultBlocks) > 0 {
				st.Transcript = append(st.Transcript, provider.Message{Role: "tool", Blocks: resultBlocks})
			}
			// A dispatched tool call always continues to the next turn:
			// the run isn't done until every tool_use has a paired
			// result AND the model has seen it.
		case ClassificationContent, ClassificationEmpty:
			// A plain reply with no tool call is normally the run's own
			// natural end (TerminalCompleted) -- but a conversational run
			// (cfg.Conversational, kernel/types.go's own doc comment on
			// RunConfig) treats this as the ordinary pause between a
			// conversation's turns instead: the model is done talking, it's
			// the human's turn next, and the SAME session picks back up via
			// ResumeConversation rather than a caller stitching separate
			// sessions together.
			if cfg.Conversational {
				k.suspendForUserInput(ctx, st, yield)
				return
			}
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
		log.Error().Err(err).Any("session_id", st.SessionID).Any("reservation_id", res.ID).Msg("kernel: cost reconciliation failed")
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
	spanCtx, genSpan := k.startSpan(ctx, "model.condense", obs.ObservationGeneration, obs.Attrs{"model.id": cfg.CondenserModelID})
	stream, serr := k.Provider.Stream(spanCtx, prompt, nil, provider.RunContext{TenantID: st.TenantID, SessionID: st.SessionID})
	if serr != nil {
		genSpan.End(obs.Attrs{"outcome": "error"})
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
	outcome := "stop"
	if streamErr != nil {
		outcome = "error"
	}
	if k.TraceContent {
		genSpan.SetContent(marshalContent(prompt), text.String())
	}
	genSpan.End(obs.Attrs{
		"model.id":             cfg.CondenserModelID,
		"usage.input_uncached": strconv.Itoa(usage.InputUncached),
		"usage.output":         strconv.Itoa(usage.OutputTokens),
		"outcome":              outcome,
	})
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
