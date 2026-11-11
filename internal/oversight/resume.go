package oversight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/runstate"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
	"github.com/truongpx396/nexus-agent-demo/internal/tools/kerneladapter"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// Resumer is the ONE place in this codebase that both decides an approval
// (via Approvals) and drives kernel.Kernel.Resume for it — composing the
// two into a single call so a REST/CLI handler (or a test) never has to get
// the sequencing right itself: decide, THEN resume, always in that order,
// always against the same approval. It is free to import kernel directly
// (only kernel's own imports are restricted, kernel/types.go's doc
// comment) — this is the mirror image of cmd/nexusd's kernelRunStarter,
// which is the only thing that drives kernel.Kernel.Run for a fresh run;
// Resumer is the only thing that drives kernel.Kernel.Resume for a
// suspended one.
//
// System/Catalog/MaxTurns mirror cmd/nexusd's kernelRunStarter fields
// exactly: this demo has no per-tenant config store yet (Phase 7's
// internal/config), so every run in the process shares one system prompt
// and one resident catalog regardless of which session it belongs to.
type Resumer struct {
	Kernel    *kernel.Kernel
	Approvals *Approvals
	Sessions  Sessions
	Keys      KeyOpener
	System    string
	Catalog   []provider.ToolSchema
	MaxTurns  int
}

// Grant approves the tool_use exactly as originally requested and resumes
// the run with it.
func (r *Resumer) Grant(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string) iter.Seq2[eventlog.Event, error] {
	ap, err := r.Approvals.Grant(ctx, tenantID, approvalID, decidedBy)
	if err != nil {
		return errSeq(err)
	}
	return r.resume(ctx, tenantID, ap, kernel.ApprovalDecisionGranted, ap.Context.Input, "")
}

// GrantModified approves the tool_use with the approver's substituted
// input and resumes the run with it.
func (r *Resumer) GrantModified(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string, modifiedInput json.RawMessage) iter.Seq2[eventlog.Event, error] {
	ap, err := r.Approvals.GrantModified(ctx, tenantID, approvalID, decidedBy, modifiedInput)
	if err != nil {
		return errSeq(err)
	}
	return r.resume(ctx, tenantID, ap, kernel.ApprovalDecisionGrantedModified, ap.Context.Input, "")
}

// Deny refuses the tool_use and resumes the run only far enough to release
// the paired synthetic result and terminate it.
func (r *Resumer) Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) iter.Seq2[eventlog.Event, error] {
	ap, err := r.Approvals.Deny(ctx, tenantID, approvalID, decidedBy, reason)
	if err != nil {
		return errSeq(err)
	}
	return r.resume(ctx, tenantID, ap, kernel.ApprovalDecisionDenied, ap.Context.Input, reason)
}

// ErrNotDecided is returned by ResumeDecided for an approval that has no decision
// to act on (still pending, expired or invalidated). Retrying cannot change that.
var ErrNotDecided = errors.New("oversight: approval has no decision to resume")

// ResumeDecided resumes the run an approval suspended, acting on the decision
// already recorded on it. A queue worker calls it after Decisions has decided
// and scheduled it. It is safe to call twice, and safe after a crash part-way:
// a call that already has its real result (a second tool_result beside the
// placeholder the suspension left) has been resumed, and nothing is done.
func (r *Resumer) ResumeDecided(ctx context.Context, tenantID, approvalID uuid.UUID) iter.Seq2[eventlog.Event, error] {
	ap, err := r.Approvals.Get(ctx, tenantID, approvalID)
	if err != nil {
		return errSeq(err)
	}
	var decision kernel.ApprovalDecisionKind
	switch ap.Status {
	case ApprovalGranted:
		decision = kernel.ApprovalDecisionGranted
	case ApprovalGrantedModified:
		decision = kernel.ApprovalDecisionGrantedModified
	case ApprovalDenied:
		decision = kernel.ApprovalDecisionDenied
	case ApprovalPending, ApprovalExpired, ApprovalInvalidated:
		return errSeq(fmt.Errorf("%w: approval %s is %s", ErrNotDecided, approvalID, ap.Status))
	default:
		return errSeq(fmt.Errorf("%w: approval %s has unknown status %q", ErrNotDecided, approvalID, ap.Status))
	}
	return r.resume(ctx, tenantID, ap, decision, ap.Context.Input, ap.Reason)
}

// answered reports whether a call already has its real result: the suspension
// leaves one placeholder tool_result, and the resume appends the second.
func answered(history []eventlog.Event, toolUseEventID uuid.UUID) bool {
	n := 0
	for _, e := range history {
		if e.Type == eventlog.EventToolResult && e.PairRef != nil && *e.PairRef == toolUseEventID {
			n++
		}
	}
	return n > 1
}

func (r *Resumer) resume(ctx context.Context, tenantID uuid.UUID, ap Approval, decision kernel.ApprovalDecisionKind, originalInput json.RawMessage, reason string) iter.Seq2[eventlog.Event, error] {
	st, sess, err := r.loadRunState(ctx, tenantID, ap.SessionID)
	if err != nil {
		return errSeq(err)
	}
	if answered(st.History, ap.ToolUseEventID) {
		return func(func(eventlog.Event, error) bool) {} // already resumed: nothing to do
	}
	cfg := runstate.ConfigFor(sess, kernel.RunConfig{System: r.System, Catalog: r.Catalog, MaxTurns: r.MaxTurns})

	res := kernel.PendingResolution{
		ToolUseEventID: ap.ToolUseEventID, ToolID: ap.ToolID, Input: originalInput,
		ApprovedDigest: ap.CanonicalDigest, Decision: decision, Reason: reason,
	}
	if decision == kernel.ApprovalDecisionGrantedModified {
		res.ModifiedInput = ap.GrantedInput
	}
	return r.Kernel.Resume(ctx, st, cfg, res)
}

// loadRunState rehydrates everything kernel.Kernel.Resume needs from the
// session's own stored history: History + Transcript (kernel.Rehydrate,
// README task 5.8's own scope note — pure structural replay, no model/
// tool/append), and a Seal closure over the session's existing active DEK
// (there is no live RunState.Seal to reuse the way a fresh run's
// handleCreateRun has one in hand — Resume runs entirely out of band, after
// the original Run call already returned).
func (r *Resumer) loadRunState(ctx context.Context, tenantID, sessionID uuid.UUID) (*kernel.RunState, eventlog.Session, error) {
	sess, history, err := r.Sessions.Load(ctx, tenantID, sessionID)
	if err != nil {
		return nil, eventlog.Session{}, err
	}

	dekCache := map[string]crypto.DEK{}
	decrypt := func(ctx context.Context, e eventlog.Event) ([]byte, error) {
		if e.KeyID == crypto.ErasureKeyID {
			return e.Payload, nil // stored plaintext — see crypto.ErasureKeyID's own doc comment
		}
		if dek, ok := dekCache[e.KeyID]; ok {
			return crypto.Open(dek, e.Payload, tenantID.String(), sessionID.String())
		}
		dek, err := r.Keys.Unwrap(ctx, tenantID, e.KeyID)
		if err != nil {
			return nil, err
		}
		dekCache[e.KeyID] = dek
		return crypto.Open(dek, e.Payload, tenantID.String(), sessionID.String())
	}

	transcript, toolUseIDs, err := kernel.Rehydrate(ctx, history, decrypt)
	if err != nil {
		return nil, eventlog.Session{}, err
	}

	// Restore this session's Rule-of-Two taint state before Kernel.Resume
	// dispatches the ONE approved/denied tool call — kernel.TaintSeeder's
	// own doc comment on why this can't just happen lazily inside the tool
	// executor itself (internal/runctl/rehydrate.go's own loadRunState
	// mirrors this exactly for its own resume paths).
	if engaged, terr := kernel.RehydrateTaint(ctx, history, decrypt); terr != nil {
		return nil, eventlog.Session{}, terr
	} else if seeder, ok := r.Kernel.Tools.(kernel.TaintSeeder); ok {
		seeder.SeedTaint(sessionID, sess.AutonomyLevel, engaged)
	}

	keyID, err := currentActiveKeyID(history)
	if err != nil {
		return nil, eventlog.Session{}, err
	}
	dek, err := r.Keys.Unwrap(ctx, tenantID, keyID)
	if err != nil {
		return nil, eventlog.Session{}, err
	}

	seal := func(plaintext []byte) (sealed, digest []byte, kid string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", fmt.Errorf("oversight: seal resumed event payload: %w", err)
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}

	return &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal, History: history, Transcript: transcript, ToolUseIDs: toolUseIDs}, sess, nil
}

func currentActiveKeyID(history []eventlog.Event) (string, error) {
	for _, h := range slices.Backward(history) {
		if h.KeyID != crypto.ErasureKeyID {
			return h.KeyID, nil
		}
	}
	return "", errors.New("oversight: session has no active key in its history")
}

func errSeq(err error) iter.Seq2[eventlog.Event, error] {
	return func(yield func(eventlog.Event, error) bool) {
		yield(eventlog.Event{}, err)
	}
}

// ExecutorFor is a small convenience cmd/nexusd can use to assert, at wiring
// time, that the configured tools.Pipeline actually implements
// kernel.ApprovedExecutor (via kerneladapter.PipelineExecutor) before handing it to
// a Resumer — Resume itself only discovers a missing implementation at the
// first call (kernel/loop.go's Resume, "does not implement
// ApprovedExecutor"), and failing at startup is friendlier than failing on
// a tenant's first approval.
func ExecutorFor(p *tools.Pipeline) kernel.ApprovedExecutor {
	return kerneladapter.PipelineExecutor{Pipeline: p}
}
