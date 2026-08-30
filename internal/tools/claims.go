package tools

import (
	"context"

	"github.com/google/uuid"
)

// ClaimOutcome is what Claims.Open reports about the (session, digest) pair
// it was just asked about.
type ClaimOutcome int

const (
	// ClaimFresh means a brand-new in_flight claim was just created — safe
	// to proceed to Tool.Call.
	ClaimFresh ClaimOutcome = iota
	// ClaimAmbiguous means a claim for this exact digest already exists and
	// is STILL in_flight — either a genuine concurrent duplicate, or one a
	// crash orphaned. Either way, task 6.6 is explicit: never re-execute.
	// It must be resolved out of band (internal/runctl.ResolveClaim) before
	// this exact call can run again.
	ClaimAmbiguous
	// ClaimDone means a claim for this exact digest already completed —
	// "completed short-circuits" (task 6.6): the call is not repeated.
	ClaimDone
)

// Claims is the write-ahead idempotency hook README task 6.6 names: Open
// durably records a canonical-digest-keyed claim in_flight BEFORE a
// non-read-only tool's effect leaves this process (finishCall's step 13,
// immediately before Tool.Call); Complete resolves it once Call returns,
// success or error — the call was attributably ATTEMPTED either way.
// Declared here rather than importing internal/store directly — the same
// decoupling idiom as DerivedArtifactRecorder/SandboxExec above — so this
// package stays free of a store/crypto dependency; cmd/nexusd wires the
// real internal/runctl-backed implementation in (internal/runctl, not
// internal/store directly, because appending EventEffectClaimed/
// EventEffectClaimResolved needs crypto to seal them, and internal/store
// cannot import internal/crypto without a cycle). Nil is valid and simply
// skips write-ahead tracking — the pre-Phase-6 behavior every existing test
// still gets.
type Claims interface {
	Open(ctx context.Context, tenantID, sessionID uuid.UUID, toolID string, digest []byte) (claimID uuid.UUID, outcome ClaimOutcome, err error)
	Complete(ctx context.Context, tenantID, sessionID, claimID uuid.UUID, failed bool, reason string) error
}
