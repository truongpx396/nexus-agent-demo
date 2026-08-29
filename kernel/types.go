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

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
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
	// before the turn loop starts. Empty for a resumed/continued run (not
	// exercised this phase — Phase 6 owns resume).
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
}
