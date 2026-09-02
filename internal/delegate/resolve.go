package delegate

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// OnChildTerminal is the one entry point every place in this codebase that
// might just have driven a session to completion should call, unconditionally
// (README task 8.10). It is a documented no-op for the overwhelming majority
// of sessions — an ordinary root run has no pending delegations row naming
// it as a child at all — and only does real work for the rare case this
// package actually owns: sessionID IS a delegation's child AND has just
// reached a terminal status.
//
// Call sites, beyond Spawn's own goroutine (spawn.go): cmd/nexusd's queue
// runner, after internal/runctl.Control.Resume drives a crash-recovered
// session to completion, and the approval Grant/Deny handlers, after
// internal/oversight.Resumer drives an approval-suspended session to
// completion — a delegated child that ITSELF suspended on an ordinary tool
// approval only reaches this function via one of those two paths, never
// Spawn's own goroutine (which already returned once the child first
// suspended).
func (d *Delegations) OnChildTerminal(ctx context.Context, tenantID, childSessionID uuid.UUID) error {
	var pending Delegation
	var found bool
	var child store.Session
	err := d.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pending, found, err = findPendingByChild(ctx, tx, childSessionID)
		if err != nil || !found {
			return err
		}
		child, err = store.GetSession(ctx, tx, childSessionID)
		return err
	})
	if err != nil || !found {
		return err
	}
	if child.Status != store.SessionStatusCompleted && child.Status != store.SessionStatusFailed {
		return nil // not terminal yet — a later call site will find it
	}

	outcome, result, reason, err := d.finalize(ctx, tenantID, pending, child)
	if err != nil {
		return err
	}

	if pending.FanoutID != nil {
		if d.cfg.Fanout == nil {
			return fmt.Errorf("delegate: child %s belongs to fanout %s but no FanoutResolver is wired", childSessionID, *pending.FanoutID)
		}
		return d.cfg.Fanout.ResolveFanout(ctx, tenantID, *pending.FanoutID)
	}
	return d.resumeParent(ctx, tenantID, pending, kernel.DelegationResolution{
		ToolUseEventID: pending.ParentToolUseEventID, ToolID: pending.AgentID,
		Outcome: outcome, Result: result, Reason: reason,
	})
}

// finalize is the part BOTH resolution paths share (README task 8.11/8.14):
// validate the child's return against its declared schema, fold its taint
// into the parent — a plain delegation's own parent session for the
// ordinary case, the PLAN's driving session for a fanout child, since
// that's what ParentSessionID names either way — and append the parent's
// own EventDelegationReturned. Only what happens AFTER differs: an ordinary
// delegation resumes one suspended kernel run (resumeParent); a fanout
// child instead asks internal/plan whether its whole cohort is done yet
// (ResolveFanout).
func (d *Delegations) finalize(ctx context.Context, tenantID uuid.UUID, pending Delegation, child store.Session) (outcome kernel.DelegationOutcomeKind, result json.RawMessage, reason string, err error) {
	result, reason, boundExceeded, err := d.extractResult(ctx, tenantID, child, pending.ReturnSchema)
	if err != nil {
		return "", nil, "", fmt.Errorf("delegate: extract result for delegation %s: %w", pending.DelegationID, err)
	}

	outcome = kernel.DelegationReturned
	status := StatusReturned
	if child.Status == store.SessionStatusFailed {
		result = nil
		if child.TerminalReason != nil {
			reason = "child session failed: " + *child.TerminalReason
		} else {
			reason = "child session failed"
		}
	}
	if boundExceeded {
		outcome = kernel.DelegationBoundExceeded
		status = StatusBoundExceeded
	}

	// Taint-ascend (task 8.11): read from the CHILD's own event-derived
	// state, never from a claim inside result — TaintStateFor is exactly
	// that read (pipeline.go's own doc comment on the honest process-
	// lifetime-cache scope note applies here too).
	var childEngaged [3]bool
	if d.cfg.Pipeline != nil {
		childEngaged = d.cfg.Pipeline.TaintStateFor(child.SessionID)
		d.cfg.Pipeline.FoldTaint(pending.ParentSessionID, childEngaged)
	}

	if err := d.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := resolveStatus(ctx, tx, pending.DelegationID, status, result, reason); err != nil {
			return err
		}
		toolID := pending.AgentID // best available label; delegate.go's own tool ref is what actually matters for the pair
		_, err := d.appendEvent(ctx, tx, tenantID, pending.ParentSessionID, store.EventTaintTransition, nil, nil, taintTransitionPayload{
			ChildSessionID: child.SessionID, Engaged: childEngaged,
		})
		if err != nil {
			return err
		}
		_, err = d.appendEvent(ctx, tx, tenantID, pending.ParentSessionID, store.EventDelegationReturned, &toolID, &pending.ParentToolUseEventID, delegationReturnedPayload{
			DelegationID: pending.DelegationID, ChildSessionID: child.SessionID, Outcome: string(outcome), Reason: reason,
		})
		return err
	}); err != nil {
		return "", nil, "", err
	}
	return outcome, result, reason, nil
}

// resumeParent rehydrates the parent's RunState and drives
// Kernel.ResumeDelegation, under the session-key lock if one is configured
// — the same acquire/release dance internal/queue.Worker's own pollOnce
// already runs, applied here so a fan-out's several children (or a child's
// return racing an operator cancel) never drive the same parent
// concurrently.
func (d *Delegations) resumeParent(ctx context.Context, tenantID uuid.UUID, pending Delegation, res kernel.DelegationResolution) error {
	var token string
	var locked bool
	if d.cfg.Lock != nil {
		var err error
		token, locked, err = d.cfg.Lock.Acquire(ctx, pending.ParentSessionID.String())
		if err != nil {
			return fmt.Errorf("delegate: acquire parent lock: %w", err)
		}
		if !locked {
			return fmt.Errorf("delegate: parent session %s is locked by another worker; will retry on the next terminal check", pending.ParentSessionID)
		}
		defer func() { _ = d.cfg.Lock.Release(ctx, pending.ParentSessionID.String(), token) }()
	}

	st, sess, err := d.loadRunState(ctx, tenantID, pending.ParentSessionID)
	if err != nil {
		return err
	}
	if sess.Status != store.SessionStatusSuspended {
		return fmt.Errorf("delegate: parent session %s is %s, not suspended; refusing to resume a delegation it isn't waiting on", pending.ParentSessionID, sess.Status)
	}
	cfg := kernel.RunConfig{System: d.cfg.System, Catalog: d.cfg.Catalog, MaxTurns: d.cfg.MaxTurns, AutonomyLevel: sess.AutonomyLevel, ModelID: sess.RouteModelID}

	for _, err := range d.cfg.Kernel.ResumeDelegation(ctx, st, cfg, res) {
		if err != nil {
			return fmt.Errorf("delegate: resume parent %s: %w", pending.ParentSessionID, err)
		}
	}
	return nil
}

// extractResult reads the child's last EventContent as its return value.
// With no declared schema, any text is accepted verbatim. With a schema
// declaring "required" top-level keys, the text must parse as a JSON object
// carrying every one of them — a deliberately light check (this codebase
// carries no JSON-Schema library), honest about what it does and does not
// verify; a failure here is what makes boundExceeded=true.
func (d *Delegations) extractResult(ctx context.Context, tenantID uuid.UUID, child store.Session, schema json.RawMessage) (result json.RawMessage, reason string, boundExceeded bool, err error) {
	text, err := d.lastContentText(ctx, tenantID, child.SessionID)
	if err != nil {
		return nil, "", false, err
	}
	if len(schema) == 0 {
		out, merr := json.Marshal(map[string]string{"output": text})
		return out, "", false, merr
	}
	var required struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &required); err != nil {
		return nil, "", false, fmt.Errorf("invalid return_schema: %w", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return nil, fmt.Sprintf("child's return was not a JSON object matching return_schema: %v", err), true, nil
	}
	for _, k := range required.Required {
		if _, ok := obj[k]; !ok {
			return nil, fmt.Sprintf("child's return is missing required field %q", k), true, nil
		}
	}
	return json.RawMessage(text), "", false, nil
}

// lastContentText decrypts the child's own final EventContent — the same
// selective-decrypt idiom store.ReplayFullProjection already uses for a
// terminal event's exact reason, applied here to a content event instead.
func (d *Delegations) lastContentText(ctx context.Context, tenantID, sessionID uuid.UUID) (string, error) {
	var history []store.Event
	err := d.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		history, err = store.ListEvents(ctx, tx, sessionID)
		return err
	})
	if err != nil {
		return "", err
	}

	var last *store.Event
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Type == store.EventContent {
			last = &history[i]
			break
		}
	}
	if last == nil {
		return "", nil
	}

	var dek crypto.DEK
	if err := d.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var uerr error
		dek, uerr = d.Keys.Unwrap(ctx, tx, last.KeyID)
		return uerr
	}); err != nil {
		return "", err
	}
	plaintext, err := crypto.Open(dek, last.Payload, tenantID.String(), sessionID.String())
	if err != nil {
		return "", err
	}
	var payload struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return "", err
	}
	return payload.Body, nil
}

// loadRunState mirrors internal/oversight.Resumer.loadRunState field-for-
// field (that file's own doc comment explains why this can't be shared:
// each out-of-band driver rehydrates independently, with no live
// RunState.Seal to reuse).
func (d *Delegations) loadRunState(ctx context.Context, tenantID, sessionID uuid.UUID) (*kernel.RunState, store.Session, error) {
	var sess store.Session
	var history []store.Event
	err := d.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
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
			return e.Payload, nil
		}
		if dek, ok := dekCache[e.KeyID]; ok {
			return crypto.Open(dek, e.Payload, tenantID.String(), sessionID.String())
		}
		var dek crypto.DEK
		if err := d.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var uerr error
			dek, uerr = d.Keys.Unwrap(ctx, tx, e.KeyID)
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

	var keyID string
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].KeyID != crypto.ErasureKeyID {
			keyID = history[i].KeyID
			break
		}
	}
	if keyID == "" {
		return nil, store.Session{}, fmt.Errorf("delegate: session %s has no active key in its history", sessionID)
	}
	var dek crypto.DEK
	if err := d.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var uerr error
		dek, uerr = d.Keys.Unwrap(ctx, tx, keyID)
		return uerr
	}); err != nil {
		return nil, store.Session{}, err
	}

	return &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: sealFuncFor(dek, tenantID, sessionID), History: history, Transcript: transcript}, sess, nil
}

type taintTransitionPayload struct {
	ChildSessionID uuid.UUID `json:"child_session_id"`
	Engaged        [3]bool   `json:"engaged"`
}

type delegationReturnedPayload struct {
	DelegationID   uuid.UUID `json:"delegation_id"`
	ChildSessionID uuid.UUID `json:"child_session_id"`
	Outcome        string    `json:"outcome"`
	Reason         string    `json:"reason,omitempty"`
}
