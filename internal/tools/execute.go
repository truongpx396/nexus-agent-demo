package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/truongpx396/nexus-agent-demo/internal/hooks"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
)

// Execute runs the 16-step pipeline (README task 3.4) for one invocation:
//
//  1. Resolve      — qualified ref lookup against the session's pinned catalog manifest, falling through to cfg.Dynamic (Phase 11) on a miss
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
	tool, descriptor, dynamic, errRes := p.resolveTool(ctx, inv.TenantID, inv.SessionID, ref)
	if errRes != nil {
		return *errRes
	}

	// Step 3: admission gate. A dynamically-resolved tool (Phase 11's
	// per-tenant MCP tools) was never Registry.Register'd, so there is no
	// cached admission verdict to look up here — resolveTool's own
	// DynamicResolver call is where that tenant's admission decision
	// (its mcp_servers row's status) was already enforced, fail-closed.
	if !dynamic {
		status, _ := p.cfg.Registry.AdmissionStatus(ref)
		if status != AdmissionClean {
			return errorResult(fmt.Sprintf("admission_%s: %q is not admitted clean", status, ref))
		}
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
	before := state.taintState.Engaged
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

	// taintChanged/taintEngaged are set on EVERY ExecuteResult this call
	// returns from here on, regardless of the decision gate below or how
	// finishCall's own steps 12-16 end — the leg was engaged for real at
	// this permission-chain resolution, whether or not the call that
	// engaged it went on to be denied, asked about, or itself fail
	// (ResolveRuleOfTwo's own doc comment). kernel/turns.go durably
	// records the transition whenever TaintChanged is true.
	taintChanged := before != result.TaintState.Engaged
	taintEngaged := result.TaintState.Engaged

	// Steps 10-11: decision gates.
	switch result.Resolution.Decision {
	case permissions.Deny:
		return ExecuteResult{
			IsError:          true,
			Reason:           fmt.Sprintf("denied at layer %s: %s", result.Resolution.Layer, result.Resolution.Reason),
			PermissionDenied: true,
			TaintChanged:     taintChanged,
			TaintEngaged:     taintEngaged,
		}
	case permissions.Ask:
		return ExecuteResult{
			IsError:          true,
			Reason:           fmt.Sprintf("approval required at layer %s: %s", result.Resolution.Layer, result.Resolution.Reason),
			AwaitingApproval: true,
			AskKind:          string(result.Resolution.AskKind),
			CanonicalDigest:  digest,
			EffectClass:      string(descriptor.EffectClass),
			TaintChanged:     taintChanged,
			TaintEngaged:     taintEngaged,
		}
	case permissions.Allow:
		// continue below
	case permissions.Defer:
		return errorResult(fmt.Sprintf("permission_chain_bug: Resolve returned a non-final Defer at layer %s", result.Resolution.Layer))
	}

	// Steps 12-16. taintChanged/taintEngaged are set on whichever of
	// finishCall's own several return points this call lands on, rather
	// than threaded through every one of them individually — they all
	// share the SAME already-resolved taint transition from step 9 above.
	out := p.finishCall(ctx, tool, ref, descriptor, input, digest, rc, inv, state)
	out.TaintChanged = taintChanged
	out.TaintEngaged = taintEngaged
	return out
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
	tool, descriptor, dynamic, errRes := p.resolveTool(ctx, inv.TenantID, inv.SessionID, ref)
	if errRes != nil {
		return *errRes
	}

	// Step 3: admission gate — see Execute's identical comment.
	if !dynamic {
		status, _ := p.cfg.Registry.AdmissionStatus(ref)
		if status != AdmissionClean {
			return errorResult(fmt.Sprintf("admission_%s: %q is not admitted clean", status, ref))
		}
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
