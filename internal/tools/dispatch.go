package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/hooks"
)

// resolveTool runs pipeline steps 1 (resolve) and 2 (digest re-verify),
// shared by Execute and ExecuteApproved. A ref present in the static,
// process-wide Manifest resolves against the Registry exactly as before —
// dynamic==false, the only path every pre-Phase-11 caller and test
// exercises. A ref the static Manifest doesn't know about falls through to
// cfg.Dynamic (nil-valid): dynamic==true tells the caller to skip the
// Registry-keyed admission check at step 3, since a dynamically-resolved
// tool was never Registry.Register'd in the first place — its own
// DynamicResolver.Resolve call is where admission was already decided.
// descriptorDigest is recomputed fresh from what Resolve just returned
// rather than compared against a separately-cached "pinned" value: for a
// dynamic tool there is no session-start pin distinct from "what the
// resolver just fetched," so step 2 here is a self-consistency check
// (nothing mutated tool between the two Descriptor() calls), not a
// staleness check — the real staleness guard for an MCP tool is its own
// content-addressed Version (README task 11.1's #15 "digest re-verification
// at use": a schema change produces a different qualified ref, which is a
// different resolution attempt entirely, not a drifted one).
func (p *Pipeline) resolveTool(ctx context.Context, tenantID, sessionID uuid.UUID, ref ToolRef) (tool Tool, descriptor Descriptor, dynamic bool, errRes *ExecuteResult) {
	if entry, ok := p.cfg.Manifest.Resolve(ref); ok {
		t, ok := p.cfg.Registry.Lookup(ref)
		if !ok {
			r := errorResult(fmt.Sprintf("unknown_tool: %q is pinned in the manifest but not registered in this process", ref))
			return nil, Descriptor{}, false, &r
		}
		d := t.Descriptor()
		if liveDigest := descriptorDigest(d); !bytes.Equal(liveDigest, entry.DescriptorDigest) {
			r := errorResult(fmt.Sprintf("descriptor_drift: %q no longer matches the digest pinned at session start", ref))
			return nil, Descriptor{}, false, &r
		}
		return t, d, false, nil
	}

	if p.cfg.Dynamic != nil {
		t, ok, err := p.cfg.Dynamic.Resolve(ctx, tenantID, sessionID, ref)
		if err != nil {
			r := errorResult("dynamic_resolve_error: " + err.Error())
			return nil, Descriptor{}, false, &r
		}
		if ok {
			return t, t.Descriptor(), true, nil
		}
	}

	r := errorResult(fmt.Sprintf("unknown_tool: %q is not in this session's pinned catalog manifest", ref))
	return nil, Descriptor{}, false, &r
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
			log.Error().Err(cerr).Any("claim_id", claimID).Str("tool_id", ref.String()).Msg("tools: failed to complete write-ahead claim")
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
