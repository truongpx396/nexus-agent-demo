package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/hooks"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
)

// Invocation is what the pipeline receives for one tool_use. It is kept
// independent of any specific caller's own request shape (a kernel
// ToolUseRequest, a future eval harness's own shape, ...) so this package
// has no reason to import a caller's package — kernel/tools_adapter.go is
// where kernel.ToolUseRequest gets translated into this.
type Invocation struct {
	TenantID      uuid.UUID
	SessionID     uuid.UUID
	ToolName      string // the qualified {ns}/{name}@{ver} string form
	Input         json.RawMessage
	AutonomyLevel string // "read_only" | "supervised" | "autonomous"; only consulted the first time a session is seen — see stateFor's doc comment
}

// ExecuteResult is the pipeline's answer for one Invocation.
type ExecuteResult struct {
	Output           json.RawMessage
	IsError          bool
	Reason           string
	PermissionDenied bool
	AwaitingApproval bool
	AskKind          string

	// ApprovalMismatch marks a result ExecuteApproved produced because the
	// digest it recomputed at resume time didn't match the digest a human
	// approved (README task 5.7) — refused, never a silent re-request.
	ApprovalMismatch bool

	// CanonicalDigest is set only alongside AwaitingApproval: the digest
	// (steps 5/8's CanonicalDigest, over {tool_id, input} as it stood at
	// the moment the chain asked) internal/oversight.Approvals.Create binds
	// an approval to, and ExecuteApproved later re-verifies at resume time.
	CanonicalDigest []byte

	// EffectClass is set only alongside AwaitingApproval: the tool's own
	// descriptor.EffectClass, folded into the ContextPackage an approver
	// sees (internal/oversight.ContextPackage) — carried here rather than
	// re-resolved later because Pipeline's own Registry/Manifest are
	// unexported, and this is the one place that already has the
	// descriptor in hand.
	EffectClass string

	// AwaitingDelegation/ChildSessionID mirror Result.AwaitingChildSessionID
	// one layer up (README task 8.10) — finishCall's own short-circuit,
	// translated by kernel/tools_adapter.go into kernel.ToolResult exactly
	// like AwaitingApproval already is.
	AwaitingDelegation bool
	ChildSessionID     uuid.UUID
}

func errorResult(reason string) ExecuteResult { return ExecuteResult{IsError: true, Reason: reason} }

// PipelineConfig is everything one Pipeline needs at construction time —
// the resident catalog, the permission chain's tenant/session-independent
// config, the hook chain's static configuration, and where oversized
// results spill to. Per-session state (autonomy, Rule-of-Two taint) is
// tracked internally, keyed by SessionID, not part of this config.
type PipelineConfig struct {
	Registry    *Registry
	Manifest    Manifest
	Chain       *permissions.Chain
	Hooks       *hooks.Dispatcher
	HookConfigs []hooks.Config
	Blobs       BlobStore

	// DerivedArtifacts, if set, tracks each blob spill (README task 5.4) so
	// internal/crypto/shred.go's erasure and reconciliation can find and
	// hard-delete it. Nil is valid — spills simply go untracked, the
	// pre-Phase-5 behavior every existing caller and test still gets.
	DerivedArtifacts DerivedArtifactRecorder

	// WorkspaceRoot is the local directory each session's filesystem-
	// touching builtin tools (file_read/file_write/file_search) are scoped
	// under, one subdirectory per SessionID — and, once SandboxFactory is
	// set, the same directory a session's sandbox bind-mounts at
	// /workspace (internal/sandbox.Config.WorkspaceDir), so both paths
	// agree on what "the session's files" means.
	WorkspaceRoot string

	// SandboxFactory, if set, returns the SandboxExec (README task 5.12)
	// platform/shell runs through for sessionID, one per invocation — cheap
	// enough to call unconditionally (internal/sandbox.SessionSandbox is
	// just a struct binding a Docker client + a per-session Config; no
	// container exists until Exec is actually called). Nil is valid and
	// leaves RunContext.Sandbox unset — the pre-Phase-5 unsandboxed
	// fallback WorkspaceRoot's own doc comment used to name as the honest
	// interim.
	SandboxFactory func(sessionID uuid.UUID) SandboxExec

	// Claims, if set, is the write-ahead idempotency hook README task 6.6
	// names — wrapped around Tool.Call for every non-read-only effect class
	// in finishCall. Nil is valid and simply skips write-ahead tracking, the
	// pre-Phase-6 behavior every existing test still gets.
	Claims Claims
}

// sessionState is the per-session facilities the pipeline can't share
// across sessions: the pinned autonomy ratchet and the accumulated
// Rule-of-Two taint projection.
type sessionState struct {
	autonomy *permissions.Autonomy

	// taintMu guards taintState across the whole read-resolve-write
	// sequence in Execute's step 9 — NOT just the final write. Two
	// concurrent calls in the same session (even against a
	// concurrency-safe tool, which never takes serialLock at all) must
	// never both read the same taintState, resolve independently, and race
	// to write back: that would silently let one call's Rule-of-Two
	// engagement clobber the other's instead of accumulating.
	taintMu    sync.Mutex
	taintState permissions.TaintState

	serialLock sync.Mutex // step 12's in-process serial slot for a non-concurrency-safe tool
}

// Pipeline is the single execution path (README task 3.4, pattern 16):
// construct once, share across every run this process serves, and call
// Execute per tool_use. It implements the shape kernel/tools_adapter.go
// wraps into a kernel.ToolExecutor — this package itself never imports
// kernel (kernel is the one allowed to depend on tools, never the reverse;
// kernel/types.go's own doc comment names the allowed direction).
type Pipeline struct {
	cfg PipelineConfig

	mu       sync.Mutex
	sessions map[uuid.UUID]*sessionState
}

func NewPipeline(cfg PipelineConfig) *Pipeline {
	return &Pipeline{cfg: cfg, sessions: map[uuid.UUID]*sessionState{}}
}

// stateFor returns (creating on first use) the per-session state for
// sessionID, pinning autonomy from autonomyLevel the first time this
// session is seen. Every subsequent call ignores autonomyLevel — Pin is a
// one-time thing (internal/permissions.Autonomy's own doc comment); a
// session's autonomy only ever moves via Tighten from here on.
func (p *Pipeline) stateFor(sessionID uuid.UUID, autonomyLevel string) *sessionState {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[sessionID]
	if !ok {
		level, err := permissions.ParseAutonomyLevel(autonomyLevel)
		if err != nil {
			level = permissions.AutonomyReadOnly // fail closed on an unrecognized level
		}
		s = &sessionState{autonomy: permissions.Pin(level)}
		p.sessions[sessionID] = s
	}
	return s
}

// TaintStateFor snapshots sessionID's current Rule-of-Two engaged legs — used
// by internal/delegate at TWO moments (README task 8.11): reading the
// PARENT's state at spawn time (to seed the child's own copy-at-spawn), and
// reading the CHILD's state at return time (to fold into the parent).
// Honest scope note, matching this codebase's own convention for a
// documented gap rather than a silent one: this is Pipeline's own
// process-lifetime cache, not a replay of a durable taint_transition event
// stream — migrations/0002_sessions.sql's own taint_state column is marked
// PROJECTION but nothing in this codebase (pre- or post-Phase-8) actually
// writes it yet, so a worker restart mid-run loses it exactly the way it
// already loses every other piece of Pipeline.sessionState. Wiring a real
// durable projection is future work, not a Phase 8 regression.
func (p *Pipeline) TaintStateFor(sessionID uuid.UUID) [3]bool {
	state := p.stateFor(sessionID, "")
	state.taintMu.Lock()
	defer state.taintMu.Unlock()
	return state.taintState.Engaged
}

// FoldTaint folds engaged (a child session's own event-derived Rule-of-Two
// legs) into sessionID's running TaintState (README task 8.11 — the
// taint-ascend rule: "a summary never clears the untrusted leg"). Callers
// (internal/delegate's return-time resolution) call this exactly once per
// resolved delegation, BEFORE any further tool_use in sessionID is
// dispatched — engagedCount only ever grows, matching layer 7's own
// ResolveRuleOfTwo semantics, so a session already at two legs that folds in
// a third is exactly as constrained afterward as if it had engaged that
// third leg itself. Creates sessionID's state (pinned to AutonomyReadOnly,
// the same fail-closed default stateFor uses for an unrecognized level) if
// this is the first thing this process has ever seen for it — a delegation
// can resolve after every OTHER call this process ever routed through
// Execute for the parent, so sessionID is not guaranteed to already have an
// entry.
func (p *Pipeline) FoldTaint(sessionID uuid.UUID, engaged [3]bool) {
	state := p.stateFor(sessionID, "")
	state.taintMu.Lock()
	defer state.taintMu.Unlock()
	for i, e := range engaged {
		if e {
			state.taintState.Engaged[i] = true
		}
	}
}

// ResetTurn forwards to the hook dispatcher's per-turn cap reset
// (internal/hooks task 3.11) — the kernel loop calls this once per turn,
// before dispatching any tool_use in that turn.
func (p *Pipeline) ResetTurn() {
	if p.cfg.Hooks != nil {
		p.cfg.Hooks.ResetTurn()
	}
}

// Execute runs the 16-step pipeline (README task 3.4) for one invocation:
//
//  1. Resolve      — qualified ref lookup against the session's pinned catalog manifest
//  2. Digest re-verify — the resolved descriptor must still match what the manifest pinned
//  3. Admission gate   — refuse dispatch unless the descriptor's cached verdict is clean
//  4. Input validation — Tool.ValidateInput against the tool's declared schema
//  5. Canonical digest (bind) — RFC 8785 JCS over {tool_id, input}
//  6. Gate 2   — the tool's own CheckPermissions (capability metadata)
//  7. PreToolUse hooks — may DENY/ASK/DEFER, or rewrite input through a path allowlist
//  8. Digest re-bind   — recompute the digest if step 7 rewrote input ("step 9a" re-verification)
//  9. Permission chain — the 10-layer total order, folding in steps 6 and 7 at their layers
//  10. Decision gate: DENY  — short-circuit with a typed, audited denial
//  11. Decision gate: ASK   — short-circuit with a typed, audited suspend request
//  12. Concurrency-safety gate — Tool.IsConcurrencySafe (seam only until Phase 6's cross-worker lock)
//  13. Call            — Tool.Call, a panic recovered into a typed error result
//  14. PostToolUse hooks — observe-only, tighten-only
//  15. Result budgeting — cap/paginate to ~25k tokens, spill overflow to the blob dir
//  16. Emit            — the final ExecuteResult the caller pairs to the tool_use
func (p *Pipeline) Execute(ctx context.Context, inv Invocation) ExecuteResult {
	// Step 1: resolve.
	ref, err := ParseToolRef(inv.ToolName)
	if err != nil {
		return errorResult("unknown_tool: " + err.Error())
	}
	entry, ok := p.cfg.Manifest.Resolve(ref)
	if !ok {
		return errorResult(fmt.Sprintf("unknown_tool: %q is not in this session's pinned catalog manifest", ref))
	}
	tool, ok := p.cfg.Registry.Lookup(ref)
	if !ok {
		return errorResult(fmt.Sprintf("unknown_tool: %q is pinned in the manifest but not registered in this process", ref))
	}

	// Step 2: digest re-verify — the live descriptor must match what was pinned.
	descriptor := tool.Descriptor()
	if liveDigest := descriptorDigest(descriptor); !bytes.Equal(liveDigest, entry.DescriptorDigest) {
		return errorResult(fmt.Sprintf("descriptor_drift: %q no longer matches the digest pinned at session start", ref))
	}

	// Step 3: admission gate.
	status, _ := p.cfg.Registry.AdmissionStatus(ref)
	if status != AdmissionClean {
		return errorResult(fmt.Sprintf("admission_%s: %q is not admitted clean", status, ref))
	}

	// Step 4: input validation.
	input := inv.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	rc := RunContext{TenantID: inv.TenantID, SessionID: inv.SessionID}
	if p.cfg.WorkspaceRoot != "" {
		rc.WorkspaceDir = filepath.Join(p.cfg.WorkspaceRoot, inv.SessionID.String())
	}
	if p.cfg.SandboxFactory != nil {
		rc.Sandbox = p.cfg.SandboxFactory(inv.SessionID)
	}
	if err := tool.ValidateInput(ctx, input, rc); err != nil {
		return errorResult("invalid_input: " + err.Error())
	}

	// Step 5: canonical digest (bind) — re-bound at step 8 if a hook
	// rewrites input; carried into the Ask branch below (steps 10-11) as
	// what internal/oversight.Approvals.Create binds an approval to
	// (README task 5.6), and re-verified by ExecuteApproved at resume time
	// (task 5.7).
	digest, err := CanonicalDigest(ref.String(), input)
	if err != nil {
		return errorResult("digest_error: " + err.Error())
	}

	// Step 6: Gate 2, the tool's own CheckPermissions.
	gate2Raw := tool.CheckPermissions(ctx, input, rc)
	gate2, err := toLayerOutcome(gate2Raw.Decision, gate2Raw.Reason)
	if err != nil {
		return errorResult("gate2_error: " + err.Error())
	}

	// Step 7: PreToolUse hooks.
	hctx := hooks.Context{
		ToolID:      ref.String(),
		Namespace:   ref.Namespace,
		EffectClass: string(descriptor.EffectClass),
		Input:       input,
	}
	hookOut := hooks.Outcome{Decision: hooks.Defer}
	if p.cfg.Hooks != nil {
		hookOut = p.cfg.Hooks.Dispatch(ctx, hooks.PreToolUse, hctx, p.cfg.HookConfigs)
	}
	hookOutcome, err := toLayerOutcome(string(hookOut.Decision), hookOut.Reason)
	if err != nil {
		return errorResult("hook_error: " + err.Error())
	}

	// Step 8: digest re-bind — a hook may have rewritten input.
	if hookOut.UpdatedInput != nil {
		input = hookOut.UpdatedInput
		digest, err = CanonicalDigest(ref.String(), input)
		if err != nil {
			return errorResult("digest_error: " + err.Error())
		}
	}

	// Step 9: the 10-layer permission chain. taintMu is held across the
	// whole read-resolve-write sequence (sessionState's doc comment) —
	// this serializes permission resolution per session, which never blocks
	// a different session's calls and is a reasonable stand-in for the
	// session-key serial lock Phase 6 (README task 6.2) ships for real.
	state := p.stateFor(inv.SessionID, inv.AutonomyLevel)
	state.taintMu.Lock()
	req := permissions.Request{
		ToolID:      ref.String(),
		Namespace:   ref.Namespace,
		EffectClass: permissions.EffectClass(descriptor.EffectClass),
		Taint:       toPermissionsTaint(tool.Taint()),
		Input:       string(input),
		Autonomy:    state.autonomy,
		HookOutcome: hookOutcome,
		Gate2:       gate2,
		TaintState:  state.taintState,
	}
	result, err := p.cfg.Chain.Resolve(ctx, req)
	if err != nil {
		state.taintMu.Unlock()
		return errorResult("permission_chain_error: " + err.Error())
	}
	state.taintState = result.TaintState
	state.taintMu.Unlock()

	// Steps 10-11: decision gates.
	switch result.Resolution.Decision {
	case permissions.Deny:
		return ExecuteResult{
			IsError:          true,
			Reason:           fmt.Sprintf("denied at layer %s: %s", result.Resolution.Layer, result.Resolution.Reason),
			PermissionDenied: true,
		}
	case permissions.Ask:
		return ExecuteResult{
			IsError:          true,
			Reason:           fmt.Sprintf("approval required at layer %s: %s", result.Resolution.Layer, result.Resolution.Reason),
			AwaitingApproval: true,
			AskKind:          string(result.Resolution.AskKind),
			CanonicalDigest:  digest,
			EffectClass:      string(descriptor.EffectClass),
		}
	case permissions.Allow:
		// continue below
	case permissions.Defer:
		return errorResult(fmt.Sprintf("permission_chain_bug: Resolve returned a non-final Defer at layer %s", result.Resolution.Layer))
	}

	// Steps 12-16.
	return p.finishCall(ctx, tool, ref, descriptor, input, digest, rc, inv, state)
}

// ExecuteApproved is Phase 5's resume-time entry point (README task 5.7):
// steps 1-5 (resolve, digest re-verify against the pinned manifest,
// admission, input validation, canonical digest) run exactly as Execute's
// do, but steps 6-11 (Gate 2, hooks, the 10-layer permission chain, the
// decision gates) are deliberately skipped — a human already authorized
// this exact digest out of band (internal/oversight.Approval), so re-asking
// the chain would be nonsensical, not just redundant. Instead, the freshly
// recomputed canonical digest is compared against approvedDigest (what the
// approval actually bound, at Create or GrantModified time); on a mismatch
// this refuses with ExecuteResult.ApprovalMismatch rather than executing —
// "never a silent re-request." A match falls into the same steps 12-16
// every ordinary call uses.
func (p *Pipeline) ExecuteApproved(ctx context.Context, inv Invocation, approvedDigest []byte) ExecuteResult {
	// Step 1: resolve.
	ref, err := ParseToolRef(inv.ToolName)
	if err != nil {
		return errorResult("unknown_tool: " + err.Error())
	}
	entry, ok := p.cfg.Manifest.Resolve(ref)
	if !ok {
		return errorResult(fmt.Sprintf("unknown_tool: %q is not in this session's pinned catalog manifest", ref))
	}
	tool, ok := p.cfg.Registry.Lookup(ref)
	if !ok {
		return errorResult(fmt.Sprintf("unknown_tool: %q is pinned in the manifest but not registered in this process", ref))
	}

	// Step 2: digest re-verify — the live descriptor must match what was pinned.
	descriptor := tool.Descriptor()
	if liveDigest := descriptorDigest(descriptor); !bytes.Equal(liveDigest, entry.DescriptorDigest) {
		return errorResult(fmt.Sprintf("descriptor_drift: %q no longer matches the digest pinned at session start", ref))
	}

	// Step 3: admission gate.
	status, _ := p.cfg.Registry.AdmissionStatus(ref)
	if status != AdmissionClean {
		return errorResult(fmt.Sprintf("admission_%s: %q is not admitted clean", status, ref))
	}

	// Step 4: input validation.
	input := inv.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	rc := RunContext{TenantID: inv.TenantID, SessionID: inv.SessionID}
	if p.cfg.WorkspaceRoot != "" {
		rc.WorkspaceDir = filepath.Join(p.cfg.WorkspaceRoot, inv.SessionID.String())
	}
	if p.cfg.SandboxFactory != nil {
		rc.Sandbox = p.cfg.SandboxFactory(inv.SessionID)
	}
	if err := tool.ValidateInput(ctx, input, rc); err != nil {
		return errorResult("invalid_input: " + err.Error())
	}

	// Step 5: canonical digest — and the check ExecuteApproved exists for.
	digest, err := CanonicalDigest(ref.String(), input)
	if err != nil {
		return errorResult("digest_error: " + err.Error())
	}
	if !bytes.Equal(digest, approvedDigest) {
		return ExecuteResult{
			IsError:          true,
			Reason:           fmt.Sprintf("approval_mismatch: the input being executed for %q no longer matches what was approved", ref),
			ApprovalMismatch: true,
		}
	}

	state := p.stateFor(inv.SessionID, inv.AutonomyLevel)
	return p.finishCall(ctx, tool, ref, descriptor, input, digest, rc, inv, state)
}

// finishCall runs steps 12-16 (concurrency gate, call, post-hooks, result
// budgeting, emit) — the tail every successful Execute or ExecuteApproved
// call shares, factored out so neither path duplicates it.
func (p *Pipeline) finishCall(ctx context.Context, tool Tool, ref ToolRef, descriptor Descriptor, input json.RawMessage, digest []byte, rc RunContext, inv Invocation, state *sessionState) ExecuteResult {
	// Step 12: concurrency-safety gate. There is no cross-worker lock yet
	// (Phase 6 task 6.2's Redis session-key lock) — a single-process
	// in-memory serial slot is the honest interim: it prevents this
	// process's own goroutines from racing a non-concurrency-safe tool
	// against itself within one session, which is the only concurrency this
	// phase's single-process demo can produce in the first place.
	if !tool.IsConcurrencySafe(input) {
		state.serialLock.Lock()
		defer state.serialLock.Unlock()
	}

	// Step 13 (write-ahead half): task 6.6's claim, opened for every
	// non-read-only effect BEFORE Tool.Call runs — "before the effect
	// leaves the process." A read-only tool never touches Claims at all: it
	// has nothing to make idempotent in the first place.
	nonReadOnly := descriptor.EffectClass != EffectClassReadOnly
	var claimID uuid.UUID
	if nonReadOnly && p.cfg.Claims != nil {
		id, outcome, err := p.cfg.Claims.Open(ctx, inv.TenantID, inv.SessionID, ref.String(), digest)
		if err != nil {
			return errorResult("claim_error: " + err.Error())
		}
		switch outcome {
		case ClaimAmbiguous:
			return errorResult(fmt.Sprintf("effect_claim_in_flight: an earlier attempt at this exact call (%s) is unresolved and must be resolved — probe or human, never re-executed — before this can run again", id))
		case ClaimDone:
			return errorResult(fmt.Sprintf("effect_claim_already_completed: this exact call already ran once (claim %s); not re-executed", id))
		case ClaimFresh:
			claimID = id
		}
	}

	// Step 13 (call).
	out, callErr := safeCall(ctx, tool, input, rc)

	if nonReadOnly && p.cfg.Claims != nil && claimID != uuid.Nil {
		failed := callErr != nil || out.IsError
		reason := out.Reason
		if callErr != nil {
			reason = callErr.Error()
		}
		if cerr := p.cfg.Claims.Complete(ctx, inv.TenantID, inv.SessionID, claimID, failed, reason); cerr != nil {
			// Best-effort: the tool's own outcome is not invalidated by a
			// bookkeeping write failing, exactly like kernel/loop.go's
			// reconcile() logs rather than fails the turn over a cost
			// reconciliation error. A claim left in_flight here is exactly
			// the ambiguous state a future retry's Open() already refuses
			// to silently re-execute past.
			slog.Error("tools: failed to complete write-ahead claim", "error", cerr, "claim_id", claimID, "tool_id", ref.String())
		}
	}

	if callErr != nil {
		return errorResult("tool_error: " + callErr.Error())
	}

	// platform/delegate's own short-circuit (README task 8.10): the effect
	// already happened (a child session is running, asynchronously, out of
	// process) — there is no ordinary Output to run through PostToolUse
	// hooks or result budgeting, and nothing to emit as a tool_result yet.
	// kernel/loop.go's dispatch loop reacts to ExecuteResult.AwaitingDelegation
	// exactly the way it reacts to AwaitingApproval: suspend, don't continue.
	if out.AwaitingChildSessionID != nil {
		return ExecuteResult{AwaitingDelegation: true, ChildSessionID: *out.AwaitingChildSessionID, EffectClass: string(descriptor.EffectClass)}
	}

	// Step 14: PostToolUse hooks — observe/tighten only.
	if p.cfg.Hooks != nil {
		postOut := p.cfg.Hooks.Dispatch(ctx, hooks.PostToolUse, hooks.Context{
			ToolID: ref.String(), Namespace: ref.Namespace, EffectClass: string(descriptor.EffectClass), Input: input,
		}, p.cfg.HookConfigs)
		switch postOut.Decision {
		case hooks.Deny:
			return ExecuteResult{IsError: true, Reason: "result withheld by a post_tool_use hook: " + postOut.Reason}
		case hooks.Ask:
			out.Reason = joinReason(out.Reason, "flagged for review by a post_tool_use hook: "+postOut.Reason)
		case hooks.Defer, hooks.Allow:
			// no change — an Allow here is exactly as inert as everywhere else (dispatcher.go's normalize already coerced it to Defer)
		}
	}

	// Step 15: result budgeting.
	budgeted, err := BudgetResult(ctx, p.cfg.Blobs, inv.TenantID, inv.SessionID, ref.String(), out.Output, p.cfg.DerivedArtifacts)
	if err != nil {
		return errorResult("result_budget_error: " + err.Error())
	}

	// Step 16: emit.
	return ExecuteResult{Output: budgeted, IsError: out.IsError, Reason: out.Reason}
}

func joinReason(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "; " + addition
}

func toPermissionsTaint(t Taint) permissions.Taint {
	return permissions.Taint{
		ReturnsUntrusted: t.ReturnsUntrusted,
		ReadsPrivateData: t.ReadsPrivateData,
		MutatesExternal:  t.MutatesExternal,
	}
}

// toLayerOutcome translates a wire-level decision string (from a Tool's own
// PermissionResult or a hooks.Outcome) into a permissions.LayerOutcome.
// Allow is refused here, at the translation boundary, with a descriptive
// error — internal/permissions.Chain.Resolve also guards against it, but
// failing at the point of translation names which precomputed layer
// produced the violation.
func toLayerOutcome(decision, reason string) (permissions.LayerOutcome, error) {
	switch permissions.Decision(decision) {
	case permissions.Deny, permissions.Ask, permissions.Defer:
		return permissions.LayerOutcome{Decision: permissions.Decision(decision), Reason: reason}, nil
	case permissions.Allow:
		return permissions.LayerOutcome{}, fmt.Errorf("a precomputed layer resolved Allow (%q), which is never valid", reason)
	default:
		return permissions.LayerOutcome{}, fmt.Errorf("unrecognized decision %q", decision)
	}
}

// safeCall recovers a panicking Tool.Call into a typed error — "a tool must
// never crash the kernel loop" (pipeline.go's step 13 doc comment above).
func safeCall(ctx context.Context, tool Tool, input json.RawMessage, rc RunContext) (result Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool panicked: %v", r)
		}
	}()
	return tool.Call(ctx, input, rc)
}
