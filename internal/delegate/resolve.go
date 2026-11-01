package delegate

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/runstate"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
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
	var child eventlog.Session
	err := d.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		pending, found, err = tx.FindPendingByChild(ctx, childSessionID)
		if err != nil || !found {
			return err
		}
		child, err = tx.GetSession(ctx, childSessionID)
		return err
	})
	if err != nil || !found {
		return err
	}
	if child.Status != eventlog.SessionStatusCompleted && child.Status != eventlog.SessionStatusFailed {
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
func (d *Delegations) finalize(ctx context.Context, tenantID uuid.UUID, pending Delegation, child eventlog.Session) (outcome kernel.DelegationOutcomeKind, result json.RawMessage, reason string, err error) {
	result, reason, boundExceeded, err := d.extractResult(ctx, tenantID, child, pending.ReturnSchema)
	if err != nil {
		return "", nil, "", fmt.Errorf("delegate: extract result for delegation %s: %w", pending.DelegationID, err)
	}

	outcome = kernel.DelegationReturned
	status := StatusReturned
	if child.Status == eventlog.SessionStatusFailed {
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

	if err := d.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		if err := tx.SetDelegationResult(ctx, pending.DelegationID, status, result, reason); err != nil {
			return err
		}
		toolID := pending.AgentID // best available label; delegate.go's own tool ref is what actually matters for the pair
		_, err := tx.Append(ctx, tenantID, pending.ParentSessionID, eventlog.EventTaintTransition, nil, nil, taintTransitionPayload{
			ChildSessionID: child.SessionID, Engaged: childEngaged,
		})
		if err != nil {
			return err
		}
		_, err = tx.Append(ctx, tenantID, pending.ParentSessionID, eventlog.EventDelegationReturned, &toolID, &pending.ParentToolUseEventID, delegationReturnedPayload{
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
	if sess.Status != eventlog.SessionStatusSuspended {
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
func (d *Delegations) extractResult(ctx context.Context, tenantID uuid.UUID, child eventlog.Session, schema json.RawMessage) (result json.RawMessage, reason string, boundExceeded bool, err error) {
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
// selective-decrypt idiom eventlog.ReplayFullProjection already uses for a
// terminal event's exact reason, applied here to a content event instead.
func (d *Delegations) lastContentText(ctx context.Context, tenantID, sessionID uuid.UUID) (string, error) {
	var history []eventlog.Event
	err := d.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		history, err = tx.ListEvents(ctx, sessionID)
		return err
	})
	if err != nil {
		return "", err
	}

	var last *eventlog.Event
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Type == eventlog.EventContent {
			last = &history[i]
			break
		}
	}
	if last == nil {
		return "", nil
	}

	plaintext, err := runstate.Decryptor[Tx](d.Store, tenantID, sessionID)(ctx, *last)
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

// loadRunState rebuilds the parent's RunState (runstate.Load) and restores its
// Rule-of-Two taint state before ResumeDelegation dispatches the parent's next
// tool call — kernel.TaintSeeder's own doc comment on why this can't just
// happen lazily inside the tool executor itself. This package already holds a
// concrete TaintFolder (d.cfg.Pipeline, already used by TaintStateFor/FoldTaint
// above) rather than going through an interface indirection.
func (d *Delegations) loadRunState(ctx context.Context, tenantID, sessionID uuid.UUID) (*kernel.RunState, eventlog.Session, error) {
	loaded, err := runstate.Load[Tx](ctx, d.Store, tenantID, sessionID)
	if err != nil {
		return nil, eventlog.Session{}, err
	}
	engaged, err := kernel.RehydrateTaint(ctx, loaded.History, loaded.Decrypt)
	if err != nil {
		return nil, eventlog.Session{}, err
	}
	if d.cfg.Pipeline != nil {
		d.cfg.Pipeline.SeedTaint(sessionID, loaded.Session.AutonomyLevel, engaged)
	}
	return loaded.State, loaded.Session, nil
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
