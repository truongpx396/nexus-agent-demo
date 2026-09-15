package kernel

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

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
	// ToolUseID is the PROVIDER-assigned tool_use id (README task 13.4,
	// closing F5) — additive field, omitted on rows written before this
	// change (they simply decode with an empty string; no upcast needed).
	// kernel/rehydrate.go relies on this to rebuild a correctly-paired
	// tool_use block, and to derive RunState.ToolUseIDs for Resume.
	ToolUseID string `json:"tool_use_id,omitempty"`
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

// awaitingInputPayload is EventAwaitingInput's payload -- empty on purpose,
// same "the event type itself is the whole signal" shape EventToolLoaded
// already has: there is nothing more specific to record about an ordinary
// conversational pause than the fact that it happened.
type awaitingInputPayload struct{}

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
	return k.appendEvent(ctx, st, store.EventToolUse, store.ActorModel, &toolID, nil, &modelID, toolUsePayload{ToolName: tu.ToolName, Input: tu.Input, ToolUseID: tu.ToolUseID})
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
