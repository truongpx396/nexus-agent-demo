package oversight

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
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
	Store     *store.Store
	Keys      *crypto.KeyStore
	System    string
	Catalog   []provider.ToolSchema
	MaxTurns  int
}

// Grant approves the tool_use exactly as originally requested and resumes
// the run with it.
func (r *Resumer) Grant(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string) iter.Seq2[store.Event, error] {
	ap, err := r.Approvals.Grant(ctx, tenantID, approvalID, decidedBy)
	if err != nil {
		return errSeq(err)
	}
	return r.resume(ctx, tenantID, ap, kernel.ApprovalDecisionGranted, ap.Context.Input, "")
}

// GrantModified approves the tool_use with the approver's substituted
// input and resumes the run with it.
func (r *Resumer) GrantModified(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy string, modifiedInput json.RawMessage) iter.Seq2[store.Event, error] {
	ap, err := r.Approvals.GrantModified(ctx, tenantID, approvalID, decidedBy, modifiedInput)
	if err != nil {
		return errSeq(err)
	}
	return r.resume(ctx, tenantID, ap, kernel.ApprovalDecisionGrantedModified, ap.Context.Input, "")
}

// Deny refuses the tool_use and resumes the run only far enough to release
// the paired synthetic result and terminate it.
func (r *Resumer) Deny(ctx context.Context, tenantID, approvalID uuid.UUID, decidedBy, reason string) iter.Seq2[store.Event, error] {
	ap, err := r.Approvals.Deny(ctx, tenantID, approvalID, decidedBy, reason)
	if err != nil {
		return errSeq(err)
	}
	return r.resume(ctx, tenantID, ap, kernel.ApprovalDecisionDenied, ap.Context.Input, reason)
}

func (r *Resumer) resume(ctx context.Context, tenantID uuid.UUID, ap Approval, decision kernel.ApprovalDecisionKind, originalInput json.RawMessage, reason string) iter.Seq2[store.Event, error] {
	st, sess, err := r.loadRunState(ctx, tenantID, ap.SessionID)
	if err != nil {
		return errSeq(err)
	}
	cfg := kernel.RunConfig{System: r.System, Catalog: r.Catalog, MaxTurns: r.MaxTurns, AutonomyLevel: sess.AutonomyLevel}

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
func (r *Resumer) loadRunState(ctx context.Context, tenantID, sessionID uuid.UUID) (*kernel.RunState, store.Session, error) {
	var sess store.Session
	var history []store.Event
	err := r.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		sess, err = store.GetSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		history, err = store.ListEvents(ctx, tx, sessionID)
		return err
	})
	if err != nil {
		return nil, store.Session{}, err
	}

	dekCache := map[string]crypto.DEK{}
	decrypt := func(ctx context.Context, e store.Event) ([]byte, error) {
		if e.KeyID == crypto.ErasureKeyID {
			return e.Payload, nil // stored plaintext — see crypto.ErasureKeyID's own doc comment
		}
		if dek, ok := dekCache[e.KeyID]; ok {
			return crypto.Open(dek, e.Payload, tenantID.String(), sessionID.String())
		}
		var dek crypto.DEK
		if err := r.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var uerr error
			dek, uerr = r.Keys.Unwrap(ctx, tx, e.KeyID)
			return uerr
		}); err != nil {
			return nil, err
		}
		dekCache[e.KeyID] = dek
		return crypto.Open(dek, e.Payload, tenantID.String(), sessionID.String())
	}

	transcript, err := kernel.Rehydrate(ctx, history, decrypt)
	if err != nil {
		return nil, store.Session{}, err
	}

	keyID, err := currentActiveKeyID(history)
	if err != nil {
		return nil, store.Session{}, err
	}
	var dek crypto.DEK
	if err := r.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var uerr error
		dek, uerr = r.Keys.Unwrap(ctx, tx, keyID)
		return uerr
	}); err != nil {
		return nil, store.Session{}, err
	}

	seal := func(plaintext []byte) (sealed, digest []byte, kid string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", fmt.Errorf("oversight: seal resumed event payload: %w", err)
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}

	return &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal, History: history, Transcript: transcript}, sess, nil
}

func currentActiveKeyID(history []store.Event) (string, error) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].KeyID != crypto.ErasureKeyID {
			return history[i].KeyID, nil
		}
	}
	return "", fmt.Errorf("oversight: session has no active key in its history")
}

func errSeq(err error) iter.Seq2[store.Event, error] {
	return func(yield func(store.Event, error) bool) {
		yield(store.Event{}, err)
	}
}

// ExecutorFor is a small convenience cmd/nexusd can use to assert, at wiring
// time, that the configured tools.Pipeline actually implements
// kernel.ApprovedExecutor (via kernel.PipelineExecutor) before handing it to
// a Resumer — Resume itself only discovers a missing implementation at the
// first call (kernel/loop.go's Resume, "does not implement
// ApprovedExecutor"), and failing at startup is friendlier than failing on
// a tenant's first approval.
func ExecutorFor(p *tools.Pipeline) kernel.ApprovedExecutor {
	return kernel.PipelineExecutor{Pipeline: p}
}
