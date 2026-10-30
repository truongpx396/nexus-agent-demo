package delegate

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The wait is for the parent's own bookkeeping — one transaction that follows
// Spawn on the same goroutine — so in practice it is milliseconds. The ceiling
// only matters when that bookkeeping failed or the parent died, and is there so
// a child goroutine can never wait forever.
const (
	bindWaitTimeout = 30 * time.Second
	bindPollMin     = 5 * time.Millisecond
	bindPollMax     = 100 * time.Millisecond
)

// awaitBound blocks until the delegation naming childSessionID can be
// resolved, which is when it is bound to the parent's gating tool_use event
// (Bind), or immediately when it never will be: a session that is no
// delegation's child at all, or a fanout child, whose parent is a plan step
// rather than a tool_use.
//
// Spawn starts the child on its own goroutine and returns; the parent then
// appends delegation_requested and Binds in a transaction of its own
// (kernel/loop.go's suspendForDelegation runs AFTER Spawn). A child that
// finishes first used to resolve against a delegation with no tool_use to pair
// its result with, and the parent's Bind then found no pending row and failed
// the parent's run. A scripted child takes milliseconds, so this was easy to
// hit; a real model makes the window narrower, not absent.
//
// An error means the delegation was still unbound when the wait ran out (or
// ctx ended): the caller must not resolve it.
func (d *Delegations) awaitBound(ctx context.Context, tenantID, childSessionID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, bindWaitTimeout)
	defer cancel()

	wait := bindPollMin
	for {
		ready, err := d.resolvable(ctx, tenantID, childSessionID)
		if err != nil {
			return fmt.Errorf("delegate: check whether child %s is bound: %w", childSessionID, err)
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("delegate: child %s finished but its delegation was never bound to a tool_use: %w", childSessionID, ctx.Err())
		case <-time.After(wait):
		}
		wait = min(wait*2, bindPollMax)
	}
}

// resolvable reports whether childSessionID's delegation, if it has one, is
// ready to be resolved.
func (d *Delegations) resolvable(ctx context.Context, tenantID, childSessionID uuid.UUID) (bool, error) {
	var ready bool
	err := d.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		ready, err = tx.Resolvable(ctx, childSessionID)
		return err
	})
	return ready, err
}
