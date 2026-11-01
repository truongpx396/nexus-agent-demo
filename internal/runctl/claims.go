package runctl

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

// claimedPayload/claimResolvedPayload are what EventEffectClaimed/
// EventEffectClaimResolved carry — deliberately minimal: which tool, and
// (for the resolved event) how it was resolved and why. Never the tool's
// own output — the event log's ordinary tool_result is still the one
// authoritative record of that (internal/store.Claim's own doc comment).
type claimedPayload struct {
	ClaimID string `json:"claim_id"`
	ToolID  string `json:"tool_id"`
}

type claimResolvedPayload struct {
	ClaimID string `json:"claim_id"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
}

// ClaimTracker implements tools.Claims (README task 6.6) against durable
// storage: the claims ledger, with the event-log append that records each claim
// or resolution in the same unit of work. cmd/nexusd wires *ClaimTracker into
// internal/tools.PipelineConfig.Claims.
type ClaimTracker struct {
	Store Store
}

// NewClaimTracker takes its Store directly rather than a *Control — cmd/nexusd
// wires a ClaimTracker into internal/tools.PipelineConfig BEFORE a Control can
// exist (Control needs a *kernel.Kernel, which needs the tool pipeline this
// tracker feeds into — building Control first would be circular).
func NewClaimTracker(st Store) *ClaimTracker { return &ClaimTracker{Store: st} }

// Open implements tools.Claims.Open.
func (t *ClaimTracker) Open(ctx context.Context, tenantID, sessionID uuid.UUID, toolID string, digest []byte) (uuid.UUID, tools.ClaimOutcome, error) {
	var claim Claim
	var created bool
	err := t.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		claim, created, err = tx.OpenOrFindClaim(ctx, tenantID, sessionID, toolID, digest)
		if err != nil {
			return err
		}
		if created {
			_, err = tx.Append(ctx, tenantID, sessionID, eventlog.EventEffectClaimed, &toolID, nil,
				claimedPayload{ClaimID: claim.ClaimID.String(), ToolID: toolID})
		}
		return err
	})
	if err != nil {
		return uuid.Nil, 0, fmt.Errorf("runctl: open claim: %w", err)
	}
	if created {
		return claim.ClaimID, tools.ClaimFresh, nil
	}
	switch claim.Status { //nolint:exhaustive // deliberately narrow: ClaimInFlight and ClaimAbandoned both fall into ClaimAmbiguous below (Open cannot tell "still genuinely in flight" from "abandoned but this exact digest was never re-opened" apart, and treats both as a refusal — the default case's own comment)
	case ClaimCompleted:
		return claim.ClaimID, tools.ClaimDone, nil
	default: // in_flight (a genuine duplicate, or one a crash orphaned) or abandoned-but-still-mapped
		return claim.ClaimID, tools.ClaimAmbiguous, nil
	}
}

// Complete implements tools.Claims.Complete — called by
// internal/tools/pipeline.go right after Tool.Call returns.
func (t *ClaimTracker) Complete(ctx context.Context, tenantID, sessionID, claimID uuid.UUID, failed bool, reason string) error {
	status := ClaimCompleted
	if failed {
		// A failed CALL does not, by itself, prove the effect never
		// happened (the tool could have thrown after the external effect
		// already landed) — but this demo's builtin tools have no
		// after-the-fact probe capability, so the honest, fail-closed
		// choice is to mark it abandoned only when the caller (finishCall)
		// already knows the call errored BEFORE any output was produced.
		// ResolveClaim is the real escape hatch for anything more nuanced
		// than that.
		status = ClaimAbandoned
	}
	return t.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		claim, err := tx.ResolveClaim(ctx, claimID, status, reason)
		if err != nil {
			return err
		}
		_, err = tx.Append(ctx, tenantID, sessionID, eventlog.EventEffectClaimResolved, &claim.ToolID, nil,
			claimResolvedPayload{ClaimID: claimID.String(), Status: string(status), Reason: reason})
		return err
	})
}

// ResolveClaim is the operator-facing (or, if this demo ever wires one, a
// probe-facing) resolution for a claim a crash left in_flight — task 6.6's
// own "resolved by a probe or a human," made real. Called via
// internal/runctl (not internal/tools) because it needs the same seal/
// append machinery Open/Complete above do.
func (c *Control) ResolveClaim(ctx context.Context, tenantID, sessionID, claimID uuid.UUID, status ClaimStatus, reason string) (Claim, error) {
	var claim Claim
	err := c.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		claim, err = tx.ResolveClaim(ctx, claimID, status, reason)
		if err != nil {
			return err
		}
		_, err = tx.Append(ctx, tenantID, sessionID, eventlog.EventEffectClaimResolved, &claim.ToolID, nil,
			claimResolvedPayload{ClaimID: claimID.String(), Status: string(status), Reason: reason})
		return err
	})
	if err != nil {
		return Claim{}, fmt.Errorf("runctl: resolve claim %s: %w", claimID, err)
	}
	return claim, nil
}
