package oversight

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// InvalidationReason names why a caller invalidated a session's outstanding
// approval/input request — the vocabulary task 5.10 lists: cancel, the
// run's own natural terminal, a reap (an orphaned suspend nobody ever
// resolved), a cost-ceiling breach, or steering into an already-suspended
// run. None of these triggers' actual call sites exist yet in this
// codebase (ReasonAborted's own doc comment in kernel/terminal.go says
// cancel is Phase 6's internal/runctl; reap and steer are Phase 6 too) —
// Invalidate ships here as a complete, directly-tested function each of
// those will call, the same way kernel.NotImplementedToolExecutor shipped
// complete two phases before Phase 3's pipeline became its caller.
type InvalidationReason string

const (
	InvalidationCancel   InvalidationReason = "cancel"
	InvalidationTerminal InvalidationReason = "terminal"
	InvalidationReap     InvalidationReason = "reap"
	InvalidationCeiling  InvalidationReason = "ceiling_breach"
	InvalidationSteer    InvalidationReason = "steer_into_suspension"
)

// syntheticResultPayload mirrors kernel.ToolResult's JSON shape (kernel/
// loop.go's toolResultPayload) field-for-field, so a synthetic tool_result
// Invalidate releases reads identically to one the kernel itself would have
// appended — duplicated rather than imported because kernel's own payload
// type is unexported (an internal implementation detail of kernel/loop.go,
// not a shared wire contract), and oversight operates entirely out of band
// from any live kernel.Run/Resume call here.
type syntheticResultPayload struct {
	IsError   bool   `json:"is_error"`
	Reason    string `json:"reason,omitempty"`
	Synthetic bool   `json:"synthetic,omitempty"`
}

// Invalidate releases the ONE pending approval a session has outstanding
// (kernel/loop.go's suspendForApproval suspends on exactly one tool_use at
// a time), marking it invalidated and appending a SYNTHETIC tool_result
// paired to its tool_use — task 5.10's own wording, "each releasing a
// paired synthetic result" — so the paired-result invariant holds
// structurally even though the run is never going to resume this call.
// Deliberately does NOT change the session's own status or append a
// terminal event: that is the caller's job (whichever future trigger this
// is), which almost always wants a DIFFERENT terminal reason than a bare
// denial (ReasonAborted for a cancel, a stuck-detector reason for a reap,
// ...). A session with nothing pending is a no-op, not an error — every
// listed trigger may legitimately fire against a session that was never
// suspended in the first place.
func (a *Approvals) Invalidate(ctx context.Context, tenantID, sessionID uuid.UUID, reason InvalidationReason) (*Approval, error) {
	var out *Approval
	err := a.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		pending, ok, err := findPendingApproval(ctx, tx, sessionID)
		if err != nil || !ok {
			return err
		}
		if err := updateApprovalStatus(ctx, tx, pending.ApprovalID, ApprovalInvalidated, "", nil, string(reason)); err != nil {
			return err
		}
		pending.Status, pending.Reason = ApprovalInvalidated, string(reason)

		toolID := pending.ToolID
		if _, err := a.appendEvent(ctx, tx, tenantID, sessionID, store.EventApprovalInvalidated, &toolID, nil,
			approvalDecisionPayload{ApprovalID: pending.ApprovalID, ToolID: pending.ToolID, Reason: string(reason)},
		); err != nil {
			return err
		}

		ref := pending.ToolUseEventID
		if _, err := a.appendEvent(ctx, tx, tenantID, sessionID, store.EventToolResult, &toolID, &ref,
			syntheticResultPayload{IsError: true, Synthetic: true, Reason: "approval_invalidated: " + string(reason)},
		); err != nil {
			return err
		}

		out = &pending
		return nil
	})
	return out, err
}

// Invalidate is Inputs' counterpart — an input request never gates a
// tool_use (task 5.9: it "carries zero authorization value"), so there is
// no synthetic tool_result to release, only the typed status transition.
// Unlike an approval, more than one input request can plausibly be
// outstanding for a session at once (nothing in this codebase suspends the
// kernel loop on one — see RequestInput's own doc comment), so this
// invalidates every pending one.
func (i *Inputs) Invalidate(ctx context.Context, tenantID, sessionID uuid.UUID, reason InvalidationReason) ([]InputRequest, error) {
	var out []InputRequest
	err := i.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, inputRequestSelectSQL+` WHERE session_id = $1 AND status = 'pending' FOR UPDATE`, sessionID)
		if err != nil {
			return fmt.Errorf("oversight: find pending input requests for session %s: %w", sessionID, err)
		}
		var pending []InputRequest
		for rows.Next() {
			ir, err := scanInputRequest(rows)
			if err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, ir)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		for _, ir := range pending {
			if err := updateInputStatus(ctx, tx, ir.InputRequestID, InputInvalidated, string(reason)); err != nil {
				return err
			}
			ir.Status, ir.Reason = InputInvalidated, string(reason)
			if _, err := i.appendEvent(ctx, tx, tenantID, sessionID, store.EventInputInvalidated, nil, nil,
				inputRequestIDPayload{InputRequestID: ir.InputRequestID},
			); err != nil {
				return err
			}
			out = append(out, ir)
		}
		return nil
	})
	return out, err
}

func findPendingApproval(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (Approval, bool, error) {
	ap, err := scanApprovalRows(tx.QueryRow(ctx, approvalSelectSQL+` WHERE session_id = $1 AND status = 'pending' FOR UPDATE`, sessionID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Approval{}, false, nil
		}
		return Approval{}, false, err
	}
	return ap, true, nil
}
