// Package kernel is THE loop (docs/constitution.md, Principle I): the only
// place control flow lives, expressed as a single generator
// (kernel/loop.go). It may import internal/{provider,tools,promptctx,store,
// cost,reliability,obs} and nothing else — tests/contract/boundaries_test.go
// enforces that this stays true even after later phases add the packages
// this file already seams for.
package kernel

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// ToolUseRequest is the kernel's view of one tool_use assembled from a
// provider turn — enough to append an EventToolUse and hand to a
// ToolExecutor.
type ToolUseRequest struct {
	ToolUseID string
	ToolName  string
	Input     json.RawMessage
}

// ToolResult is what a ToolExecutor returns for one ToolUseRequest.
type ToolResult struct {
	Output    json.RawMessage
	IsError   bool
	Reason    string // set when IsError, or to explain a Synthetic result
	Synthetic bool

	// PermissionDenied marks a result the permission chain's final DENY
	// produced (internal/permissions, Phase 3 task 3.6) — the loop's
	// dispatch step (kernel/loop.go) reacts to this by terminating the run
	// with TerminalPermissionDenied instead of continuing to the next turn:
	// a denial is fatal to the run, not just to this one call.
	PermissionDenied bool

	// AwaitingApproval marks a result the chain's final ASK produced with no
	// standing scope to satisfy it. Phase 3 ships no oversight service
	// (that's Phase 5's internal/oversight) to act on an ASK, so the loop
	// reacts by suspending the run — appending an EventApprovalRequested and
	// marking the session's status suspended — rather than terminating or
	// continuing; resuming a suspended run is Phase 5/6's concern, not this
	// one's.
	AwaitingApproval bool
	AskKind          string // "once" | "session" | "multi_party" — set only when AwaitingApproval

	// CanonicalDigest is set only alongside AwaitingApproval — mirrors
	// tools.ExecuteResult.CanonicalDigest (README task 5.6): what
	// internal/oversight.Approvals.Create binds an approval to.
	CanonicalDigest []byte

	// ApprovalMismatch marks a result ExecuteApproved (via the optional
	// ApprovedExecutor interface below) produced because the digest it
	// recomputed at resume time didn't match what a human approved
	// (README task 5.7) — refused, never a silent re-request.
	ApprovalMismatch bool

	// EffectClass is set only alongside AwaitingApproval — mirrors
	// tools.ExecuteResult.EffectClass.
	EffectClass string

	// AwaitingDelegation marks a result the `delegate` tool produced after
	// it already spawned a child session (README task 8.10): the effect
	// (the child running) has already started, asynchronously, by the time
	// this is set — it is never resolved by a human decision the way
	// AwaitingApproval is. The loop reacts by suspending the run exactly
	// the same shape as an approval suspend (append an event, mark the
	// session suspended) but through a DIFFERENT resolution path
	// (ResumeDelegation, not Resume): a delegation is resolved by the CHILD
	// reaching its own terminal state, never by ExecuteApproved re-running
	// Tool.Call.
	AwaitingDelegation bool
	// ChildSessionID is set only alongside AwaitingDelegation: the session
	// internal/delegate just created and started running independently.
	ChildSessionID uuid.UUID
}

// ExecContext carries the per-run facts a ToolExecutor needs beyond one
// call's own request: identifiers for scoping/audit, and the session's
// pinned autonomy level (permission chain layer 3). It is a small local
// type rather than internal/permissions.AutonomyLevel because this
// package's own doc comment restricts its imports to
// internal/{provider,tools,promptctx,store,cost,reliability,obs} —
// permissions is downstream of tools, not a package kernel names directly.
type ExecContext struct {
	TenantID      uuid.UUID
	SessionID     uuid.UUID
	AutonomyLevel string
}

// ToolExecutor runs one tool_use to completion. internal/tools/pipeline.go
// (Phase 3, README task 3.4) is the real implementation; this seam exists so
// the loop's dispatch step compiles and appends a correctly paired result
// now, even though nothing can actually execute a tool until Phase 3 lands.
type ToolExecutor interface {
	Execute(ctx context.Context, req ToolUseRequest, rc ExecContext) ToolResult
}

// ApprovedExecutor is the optional interface a ToolExecutor may also
// implement to resume the ONE tool call an approval/input-request
// suspended a run on (README task 5.7/5.8) — mirrors the
// `interface{ ResetTurn() }` optional-interface check kernel/loop.go
// already does for the hook chain's per-turn cap, so an executor with
// nothing to resume (kernel.NotImplementedToolExecutor) needs no method for
// it. kernel.PipelineExecutor (tools_adapter.go) implements this by calling
// tools.Pipeline.ExecuteApproved.
type ApprovedExecutor interface {
	ExecuteApproved(ctx context.Context, req ToolUseRequest, approvedDigest []byte, rc ExecContext) ToolResult
}

// NotImplementedToolExecutor is the only ToolExecutor Phase 2 ships. Every
// call returns a synthetic error result — the paired-result invariant holds
// even though no tool actually runs yet.
type NotImplementedToolExecutor struct{}

func (NotImplementedToolExecutor) Execute(_ context.Context, _ ToolUseRequest, _ ExecContext) ToolResult {
	return ToolResult{
		IsError:   true,
		Synthetic: true,
		Reason:    "tool pipeline not implemented until Phase 3 (internal/tools/pipeline.go)",
	}
}

// BudgetGate is consulted before every Provider.Stream call — the "reserve"
// step in README task 2.1's loop order (hygiene -> reserve -> stream ->
// ... -> reconcile) — and again after the stream completes, to reconcile
// the reservation against real usage. internal/cost.Gate (Phase 4, README
// task 4.4) is the real reserve-then-reconcile implementation; this
// interface uses internal/cost's own request/response types directly
// (rather than a kernel-local translation, the way ToolExecutor needs one
// for internal/tools' richer Invocation/ExecuteResult shapes) because
// kernel is allowed to import internal/cost outright (this package's own
// doc comment) and cost.Gate's constructor-level shape already matches
// what the loop needs one-for-one.
type BudgetGate interface {
	Reserve(ctx context.Context, req cost.ReserveRequest) (cost.Reservation, error)
	Reconcile(ctx context.Context, res cost.Reservation, usage provider.Usage, reported bool) error
}

// NoopBudgetGate is a BudgetGate that never enforces anything — every
// Reserve call resolves cost.DecisionSkip and Reconcile is a no-op. It
// exists for tests and any call site that doesn't care about cost
// governance, not as Phase 2's production default anymore (cmd/nexusd
// wires the real internal/cost.Gate as of Phase 4).
type NoopBudgetGate struct{}

func (NoopBudgetGate) Reserve(_ context.Context, req cost.ReserveRequest) (cost.Reservation, error) {
	return cost.Reservation{
		ID: uuid.New(), TenantID: req.TenantID, SessionID: req.SessionID, ModelID: req.ModelID,
		Decision: cost.Decision{Kind: cost.DecisionSkip, Reason: "NoopBudgetGate: cost governance not wired"},
	}, nil
}

func (NoopBudgetGate) Reconcile(context.Context, cost.Reservation, provider.Usage, bool) error {
	return nil
}

// SealFunc seals one event's plaintext payload, returning the sealed bytes,
// a digest over the plaintext (survives crypto-shredding, FR-081), and the
// key id it was sealed under. The loop depends on this function type rather
// than internal/crypto directly, so kernel/hygiene.go and its property test
// stay free of any crypto or DB setup.
type SealFunc func(plaintext []byte) (sealed, digest []byte, keyID string, err error)

// ReceiptFunc extends the tenant's hash-chained audit receipt
// (internal/audit.Chain.Append, Phase 5, README task 5.2) for one event
// that store.Append just durably appended — called from inside the SAME
// transaction, so an event is never observable without a receipt for it
// ("day one, co-equal perimeter controls," not a retrofit). Declared here
// rather than imported from internal/audit, exactly like SealFunc above:
// this package's own doc comment restricts its imports to
// internal/{provider,tools,promptctx,store,cost,reliability,obs}, and
// internal/audit isn't on that list. cmd/nexusd wires the real
// internal/audit.Chain.Append in; a nil ReceiptFunc (every pre-Phase-5 test)
// simply means no receipt is written, not an error.
type ReceiptFunc func(ctx context.Context, tx pgx.Tx, e store.Event) error

// RunConfig is everything one Run needs beyond the growing event history.
// ModelID is the internal/provider/router.go decision, persisted onto the
// session and stamped on model-produced events for audit — Phase 2 computes
// and records it but dispatches every call through the one configured
// Provider regardless of its value (per-model provider selection is later-
// phase routing infrastructure, not a Phase 2 task).
type RunConfig struct {
	System   string
	Catalog  []provider.ToolSchema
	ModelID  string
	MaxTurns int
	// Input, if non-empty, is appended as the run's opening EventUserMessage
	// before the turn loop starts. Empty for a resumed/continued run —
	// Resume (loop.go, README task 5.8) never touches this field; general
	// crash/steer resume from an arbitrary point is still Phase 6's
	// internal/runctl + the real Checkpoint artifact.
	Input string
	// AutonomyLevel is the session's pinned autonomy level ("read_only" |
	// "supervised" | "autonomous"), forwarded to every ToolExecutor.Execute
	// call this run makes (Phase 3's permission chain layer 3). Empty
	// defaults to "supervised" at the executor, matching
	// store.CreateSession's own default.
	AutonomyLevel string
	// LoadedTools is the resident catalog's qualified tool refs, pinned
	// into this session's manifest at session start (internal/tools/
	// manifest.go, README task 3.2). Run appends one EventToolLoaded per
	// entry into the volatile zone before the turn loop starts — "tool_loaded
	// lands in the volatile zone" (task 3.2's own wording) rather than being
	// baked into the byte-stable system-prompt prefix.
	LoadedTools []string
	// CondenserModelID is the cheaper model structured compaction runs
	// under (README task 7.11, Kernel.CondenseThresholdBytes' own doc
	// comment) — empty is valid (the provider port decides what an empty
	// model id means, exactly like ModelID's own zero value); condensation
	// itself only ever runs when CondenseThresholdBytes > 0.
	CondenserModelID string
	// MemorySources names the file-first memory files already folded into
	// System by the caller (internal/memory.Store.Load, README task 7.1) —
	// Run appends one EventMemoryLoaded audit record per session naming
	// them, mirroring LoadedTools/EventToolLoaded above. Empty means no
	// memory was loaded (a tenant with nothing on disk yet, or a
	// resumed/continued run — memory is only ever injected at a fresh
	// session's start, matching "writes take effect next session").
	MemorySources []string
}

// SuspendRequest is everything OnSuspend needs to durably record an
// approval bound to the tool_use a run just suspended on.
type SuspendRequest struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	// ToolUseEventID is the tool_use this approval gates — the PairRef
	// target Resume later attaches its tool_result to.
	ToolUseEventID uuid.UUID
	// ApprovalEventID is the EventID of the EventApprovalRequested
	// suspendForApproval just appended, in case a caller wants to link the
	// two records explicitly.
	ApprovalEventID uuid.UUID
	ToolID          string
	// Input is the tool_use's PLAINTEXT original input, still held in
	// memory from the live turn — never re-read from the sealed event, so
	// OnSuspend needs no decrypt path of its own.
	Input           json.RawMessage
	CanonicalDigest []byte
	AskKind         string
	EffectClass     string
}

// OnSuspend is called once, right after suspendForApproval durably appends
// EventApprovalRequested and marks the session suspended —
// internal/oversight.Approvals.Create (Phase 5) is wired here from
// cmd/nexusd, so an approval_requested event is never left without a
// matching approvals row to eventually resolve it against. Declared here
// rather than imported from internal/oversight for the same import-
// allowlist reason as ReceiptFunc above. Nil is valid (every pre-Phase-5
// test) and simply means nothing durable beyond the event itself is
// recorded.
type OnSuspend func(ctx context.Context, tx pgx.Tx, req SuspendRequest) error

// ApprovalDecisionKind is the resolution internal/oversight (Phase 5)
// reached for the tool_use a run suspended on — what Resume acts on.
type ApprovalDecisionKind string

const (
	ApprovalDecisionGranted         ApprovalDecisionKind = "granted"
	ApprovalDecisionGrantedModified ApprovalDecisionKind = "granted_modified"
	ApprovalDecisionDenied          ApprovalDecisionKind = "denied"
	ApprovalDecisionInvalidated     ApprovalDecisionKind = "invalidated"
)

// PendingResolution is what internal/oversight hands Kernel.Resume: the
// pending tool_use this run suspended on, oversight's decision, and the
// digest that decision is bound to. ModifiedInput is set only for
// ApprovalDecisionGrantedModified; every other decision executes (or
// refuses to execute) the tool_use's own original Input.
type PendingResolution struct {
	ToolUseEventID uuid.UUID
	ToolID         string
	Input          json.RawMessage
	ModifiedInput  json.RawMessage
	ApprovedDigest []byte
	Decision       ApprovalDecisionKind
	Reason         string // populated for Denied/Invalidated
}

// DelegateSuspendRequest is everything OnDelegate needs to durably bind the
// delegation internal/tools/builtin.Delegate's Call already created (as a
// pending row with no tool_use_event_id yet) to the tool_use it gates —
// mirrors SuspendRequest field-for-field, minus the approval-specific
// digest/effect-class fields a delegation has no use for (a delegation is
// never re-verified against a canonical digest the way ExecuteApproved
// re-verifies an approval).
type DelegateSuspendRequest struct {
	TenantID       uuid.UUID
	SessionID      uuid.UUID
	ToolUseEventID uuid.UUID
	// DelegationEventID is the EventID of the EventDelegationRequested
	// suspendForDelegation just appended.
	DelegationEventID uuid.UUID
	ToolID            string
	ChildSessionID    uuid.UUID
}

// OnDelegate is called once, right after suspendForDelegation durably
// appends EventDelegationRequested and marks the session suspended —
// internal/delegate.Delegations.Bind is wired here from cmd/nexusd, so a
// delegation_requested event is never left without a matching delegations
// row bound to the tool_use it gates. Nil is valid (every pre-Phase-8 test)
// and simply means nothing beyond the event itself is recorded.
type OnDelegate func(ctx context.Context, tx pgx.Tx, req DelegateSuspendRequest) error

// DelegationOutcomeKind is how a delegation this run suspended on was
// resolved — what ResumeDelegation acts on. Unlike ApprovalDecisionKind,
// none of these are a human's decision: a delegation resolves by the CHILD
// reaching its own terminal state (Returned), by an operator/reliability
// trigger reaping an orphaned one (Reaped), or by the child's own return
// failing its declared acceptance criterion (BoundExceeded, task 8.14 —
// "non-retryable").
type DelegationOutcomeKind string

const (
	DelegationReturned      DelegationOutcomeKind = "returned"
	DelegationReaped        DelegationOutcomeKind = "reaped"
	DelegationBoundExceeded DelegationOutcomeKind = "bound_exceeded"
)

// DelegationResolution is what internal/delegate hands Kernel.ResumeDelegation:
// the pending tool_use this run suspended on, and the child's outcome.
// Result is the child's own validated return value (task 8.14: validated
// against its declared return_schema BEFORE this is ever constructed) —
// folded into the paired tool_result exactly like any other tool's output,
// never treated as instructions.
//
// The taint-ascend fold itself (README task 8.11 — the parent's own
// taint_state projection folding in the child's event-derived one) does NOT
// happen here: it happens in internal/delegate, BEFORE ResumeDelegation is
// ever called, via tools.Pipeline.FoldTaint plus its own EventTaintTransition
// append — kernel's own permission-chain/Rule-of-Two state
// (internal/tools/pipeline.go's sessionState) is private to that package,
// and kernel has no reason to import internal/permissions just to shuttle a
// [3]bool through this struct.
type DelegationResolution struct {
	ToolUseEventID uuid.UUID
	ToolID         string
	Outcome        DelegationOutcomeKind
	Result         json.RawMessage // set only for DelegationReturned
	Reason         string          // set for Reaped/BoundExceeded
}
