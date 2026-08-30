package runctl

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
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
// storage: internal/store's claims-table CRUD (claims.go), wrapped with the
// event-log appends that CRUD alone can't perform (see this package's own
// doc comment). cmd/nexusd wires *ClaimTracker into
// internal/tools.PipelineConfig.Claims.
type ClaimTracker struct {
	deps
}

// NewClaimTracker takes its collaborators directly (Store/Keys/Chain)
// rather than a *Control — cmd/nexusd wires a ClaimTracker into
// internal/tools.PipelineConfig BEFORE a Control can exist (Control needs a
// *kernel.Kernel, which needs the tool pipeline this tracker feeds into —
// building Control first would be circular).
func NewClaimTracker(st *store.Store, keys *crypto.KeyStore, chain *audit.Chain) *ClaimTracker {
	return &ClaimTracker{deps: deps{Store: st, Keys: keys, Chain: chain}}
}

// Open implements tools.Claims.Open.
func (t *ClaimTracker) Open(ctx context.Context, tenantID, sessionID uuid.UUID, toolID string, digest []byte) (uuid.UUID, tools.ClaimOutcome, error) {
	var claim store.Claim
	var created bool
	err := t.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		claim, created, err = store.OpenOrFindClaim(ctx, tx, tenantID, sessionID, toolID, digest)
		if err != nil {
			return err
		}
		if created {
			_, err = t.appendEvent(ctx, tx, tenantID, sessionID, store.EventEffectClaimed, &toolID, nil,
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
	case store.ClaimCompleted:
		return claim.ClaimID, tools.ClaimDone, nil
	default: // in_flight (a genuine duplicate, or one a crash orphaned) or abandoned-but-still-mapped
		return claim.ClaimID, tools.ClaimAmbiguous, nil
	}
}

// Complete implements tools.Claims.Complete — called by
// internal/tools/pipeline.go right after Tool.Call returns.
func (t *ClaimTracker) Complete(ctx context.Context, tenantID, sessionID, claimID uuid.UUID, failed bool, reason string) error {
	status := store.ClaimCompleted
	if failed {
		// A failed CALL does not, by itself, prove the effect never
		// happened (the tool could have thrown after the external effect
		// already landed) — but this demo's builtin tools have no
		// after-the-fact probe capability, so the honest, fail-closed
		// choice is to mark it abandoned only when the caller (finishCall)
		// already knows the call errored BEFORE any output was produced.
		// ResolveClaim is the real escape hatch for anything more nuanced
		// than that.
		status = store.ClaimAbandoned
	}
	return t.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		claim, err := store.ResolveClaim(ctx, tx, claimID, status, reason)
		if err != nil {
			return err
		}
		_, err = t.appendEvent(ctx, tx, tenantID, sessionID, store.EventEffectClaimResolved, &claim.ToolID, nil,
			claimResolvedPayload{ClaimID: claimID.String(), Status: string(status), Reason: reason})
		return err
	})
}

// ResolveClaim is the operator-facing (or, if this demo ever wires one, a
// probe-facing) resolution for a claim a crash left in_flight — task 6.6's
// own "resolved by a probe or a human," made real. Called via
// internal/runctl (not internal/tools) because it needs the same seal/
// append machinery Open/Complete above do.
func (c *Control) ResolveClaim(ctx context.Context, tenantID, sessionID, claimID uuid.UUID, status store.ClaimStatus, reason string) (store.Claim, error) {
	d := c.deps()
	var claim store.Claim
	err := c.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		claim, err = store.ResolveClaim(ctx, tx, claimID, status, reason)
		if err != nil {
			return err
		}
		_, err = d.appendEvent(ctx, tx, tenantID, sessionID, store.EventEffectClaimResolved, &claim.ToolID, nil,
			claimResolvedPayload{ClaimID: claimID.String(), Status: string(status), Reason: reason})
		return err
	})
	if err != nil {
		return store.Claim{}, fmt.Errorf("runctl: resolve claim %s: %w", claimID, err)
	}
	return claim, nil
}
