package runctl

import (
	"context"
	"fmt"
	"iter"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// ErrUnresolvedClaims is returned by Resume when sessionID has one or more
// in_flight claims — task 6.6's own guarantee, enforced one layer up from
// internal/tools/pipeline.go's own per-call refusal: rather than let the
// turn loop re-dispatch into an ambiguous external effect and rely SOLELY
// on the pipeline's digest check to catch it, Resume refuses to even start
// until every claim is resolved (internal/runctl.ResolveClaim) — belt and
// suspenders, the same layering internal/permissions' own chain uses
// throughout (README.md §4's own "ALWAYS evaluated" layers).
type ErrUnresolvedClaims struct {
	SessionID uuid.UUID
	ClaimIDs  []uuid.UUID
}

func (e ErrUnresolvedClaims) Error() string {
	return fmt.Sprintf("runctl: session %s has %d unresolved claim(s); resolve them (ResolveClaim) before resuming", e.SessionID, len(e.ClaimIDs))
}

// Resume is task 6.9's third named operation: the GENERAL crash/steer
// resume from an arbitrary point, as opposed to
// internal/oversight.Resumer's own narrower resume (scoped only to
// resolving the ONE tool_use an approval suspended a run on). It is what
// picks a session back up after `kill -9` (README §6's demo line): rehydrate
// History/Transcript from the session's own durable log, refuse if any
// claim is still ambiguous, then re-enter the turn loop via
// kernel.Kernel.Continue — whose own first step, Hygiene, synthesizes a
// result for whatever tool_use a prior process left unpaired, never
// re-executing it.
func (c *Control) Resume(ctx context.Context, tenantID, sessionID uuid.UUID) iter.Seq2[store.Event, error] {
	return func(yield func(store.Event, error) bool) {
		var claimIDs []uuid.UUID
		if err := c.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			claims, err := store.ListInFlightClaims(ctx, tx, sessionID)
			if err != nil {
				return err
			}
			for _, cl := range claims {
				claimIDs = append(claimIDs, cl.ClaimID)
			}
			return nil
		}); err != nil {
			yield(store.Event{}, err)
			return
		}
		if len(claimIDs) > 0 {
			yield(store.Event{}, ErrUnresolvedClaims{SessionID: sessionID, ClaimIDs: claimIDs})
			return
		}

		st, sess, err := c.loadRunState(ctx, tenantID, sessionID)
		if err != nil {
			yield(store.Event{}, err)
			return
		}
		if sess.Status == store.SessionStatusCompleted || sess.Status == store.SessionStatusFailed {
			yield(store.Event{}, fmt.Errorf("runctl: session %s is already terminal (%s); nothing to resume", sessionID, sess.Status))
			return
		}
		if sess.Status == store.SessionStatusSuspended {
			yield(store.Event{}, fmt.Errorf("runctl: session %s is suspended on a pending approval or input request; resolve it (internal/oversight) rather than calling Resume", sessionID))
			return
		}

		cfg := kernel.RunConfig{System: c.System, Catalog: c.Catalog, MaxTurns: c.MaxTurns, AutonomyLevel: sess.AutonomyLevel, ModelID: sess.RouteModelID}
		for ev, err := range c.Kernel.Continue(ctx, st, cfg) {
			if !yield(ev, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}
