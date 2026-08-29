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
		maxTurns := cfg.MaxTurns
		if maxTurns <= 0 {
			maxTurns = defaultMaxTurns
		}

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

		for turn := 1; ; turn++ {
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

			prompt, _ := promptctx.Build(cfg.System, cfg.Catalog, st.Transcript)

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
						k.suspendForApproval(ctx, st, yield, toolUseEvents[i].ToolID, result)
						return
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
	yield(ev, nil)
}

// suspendForApproval is the loop's reaction to an AwaitingApproval result
// (Phase 3's permission chain resolving ASK with no standing scope to
// satisfy it): append an EventApprovalRequested and mark the session
// suspended, then stop the generator WITHOUT a terminal event — a suspended
// run is paused, not done. Turning the appended event into an actual
// grant/deny decision is Phase 5's internal/oversight; resuming the run
// from here is Phase 6's internal/runctl + checkpoint. Both are seams this
// phase deliberately stops short of, mirroring how kernel.NotImplementedToolExecutor
// stopped short of Phase 3 in the same file two phases ago.
func (k *Kernel) suspendForApproval(ctx context.Context, st *RunState, yield func(store.Event, error) bool, toolID *string, result ToolResult) {
	ev, err := k.appendApprovalRequested(ctx, st, toolID, result.Reason, result.AskKind)
	if err != nil {
		yield(store.Event{}, err)
		return
	}
	if err := k.updateStatus(ctx, st, store.SessionStatusSuspended, nil); err != nil {
		yield(ev, fmt.Errorf("approval_requested event %s appended but session status update failed: %w", ev.EventID, err))
		return
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

type toolUsePayload struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

type toolResultPayload struct {
	Output           json.RawMessage `json:"output,omitempty"`
	IsError          bool            `json:"is_error"`
	Reason           string          `json:"reason,omitempty"`
	Synthetic        bool            `json:"synthetic,omitempty"`
	PermissionDenied bool            `json:"permission_denied,omitempty"`
	AwaitingApproval bool            `json:"awaiting_approval,omitempty"`
	AskKind          string          `json:"ask_kind,omitempty"`
}

type approvalRequestedPayload struct {
	ToolID  string `json:"tool_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
	AskKind string `json:"ask_kind,omitempty"`
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
		return aerr
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
