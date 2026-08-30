package runctl

import (
	"bytes"
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/harness"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// ForkOverrides is what a fork may change relative to its parent — the
// "declared overrides" README task 6.11 names. Empty fields inherit the
// parent's own value.
type ForkOverrides struct {
	ModelID             string
	SystemPromptVersion string
}

// ForkResult is what Fork reports.
type ForkResult struct {
	SessionID uuid.UUID

	// ParentDigest/ChildDigest/DigestDiverged are task 6.11's own named
	// requirement: "reports digest divergence rather than presenting it as
	// a reproduction." A fork whose harness_digest differs from its
	// parent's ran under different behavior-determining config — a
	// candidate FIX, not a byte-identical replay of the original failure —
	// and a caller must never present the two as interchangeable.
	ParentDigest   []byte
	ChildDigest    []byte
	DigestDiverged bool

	InheritedTranscript []provider.Message
}

// Fork is task 6.11: a new session from parentSessionID's history up to and
// including atSeq, with declared overrides, EXTERNAL EFFECTS DISABLED, no
// inherited approvals, and its own budget and audit chain.
//
//   - External effects disabled is enforced through EXISTING machinery, not
//     a new bypass-prone flag: the forked session's autonomy is pinned to
//     read_only regardless of the parent's level, and layer 3 of the
//     permission chain (internal/permissions.Autonomy.Resolve) already
//     denies any non-read-only effect outright at that level — the same
//     gate every ordinary read_only session goes through.
//   - No inherited approvals falls out for free: internal/oversight's
//     approvals table is keyed by session_id, and the child gets a BRAND
//     NEW one.
//   - Its own budget and audit chain likewise falls out for free:
//     internal/cost.Gate and internal/audit.Chain both key their state by
//     session_id (and tenant_id), never inherited across a fork.
//
// The child's transcript is NOT a copy of the parent's own events — the
// parent's events stay exactly where they are, referenced only by the new
// session's forked_from_session_id/fork_seq columns (migrations/
// 0002_sessions.sql, seamed since Phase 1) — Fork returns the REHYDRATED
// text a caller needs to seed a fresh kernel.RunState.Transcript for the new
// session's own first turn.
func (c *Control) Fork(ctx context.Context, tenantID, parentSessionID uuid.UUID, atSeq int64, overrides ForkOverrides) (ForkResult, error) {
	var parentSess store.Session
	var history []store.Event
	if err := c.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		parentSess, err = store.GetSession(ctx, tx, parentSessionID)
		if err != nil {
			return err
		}
		history, err = store.ListEvents(ctx, tx, parentSessionID)
		return err
	}); err != nil {
		return ForkResult{}, fmt.Errorf("runctl: fork: %w", err)
	}

	var trimmed []store.Event
	for _, e := range history {
		if e.Seq > atSeq {
			break
		}
		trimmed = append(trimmed, e)
	}
	if len(trimmed) == 0 {
		return ForkResult{}, fmt.Errorf("runctl: fork: session %s has no events at or before seq %d", parentSessionID, atSeq)
	}

	transcript, err := kernel.Rehydrate(ctx, trimmed, c.decryptFuncFor(tenantID, parentSessionID))
	if err != nil {
		return ForkResult{}, fmt.Errorf("runctl: fork: rehydrate parent transcript: %w", err)
	}

	systemPromptVersion := overrides.SystemPromptVersion
	if systemPromptVersion == "" {
		systemPromptVersion = "phase2-v1" // matches internal/surfaces/rest/server.go's own hardcoded default — see that file's own doc comment on why: no per-tenant config store exists yet
	}
	childDigest := harness.Digest(harness.Config{
		SystemPromptVersion:   systemPromptVersion,
		CatalogManifestDigest: c.CatalogManifestDigest,
		PromptMode:            "phase2-single-shot",
	})
	diverged := !bytes.Equal(parentSess.HarnessDigest, childDigest)

	modelID := overrides.ModelID
	if modelID == "" {
		modelID = parentSess.RouteModelID
	}

	newSessionID := uuid.New()
	err = c.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		dek, err := c.Keys.NewDEK(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		_ = dek // the new session's DEK is minted so its own events can be sealed once a caller starts running it; Fork itself appends nothing to the child's log
		return store.CreateSession(ctx, tx, store.Session{
			SessionID: newSessionID, SessionKey: newSessionID.String(), TenantID: tenantID,
			SurfaceID: parentSess.SurfaceID, UserID: parentSess.UserID,
			AgentID: parentSess.AgentID, AgentVersion: parentSess.AgentVersion,
			HarnessDigest: childDigest,
			DataLabel:     parentSess.DataLabel, RouteModelID: modelID,
			AutonomyLevel:       "read_only",
			ForkedFromSessionID: &parentSessionID,
			ForkSeq:             &atSeq,
			ForkOverrides:       map[string]string{"model_id": overrides.ModelID, "system_prompt_version": overrides.SystemPromptVersion},
		})
	})
	if err != nil {
		return ForkResult{}, fmt.Errorf("runctl: fork: create child session: %w", err)
	}

	return ForkResult{
		SessionID: newSessionID, ParentDigest: parentSess.HarnessDigest, ChildDigest: childDigest,
		DigestDiverged: diverged, InheritedTranscript: transcript,
	}, nil
}
