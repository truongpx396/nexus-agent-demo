package runctl

import (
	"context"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// SettledObserver is told that a worker has driven a session as far as it will
// go this time: it completed, failed, suspended on a human, or is waiting for
// the next message. It is how a feature that cares what became of a session it
// does not run itself finds out, without runctl — or whoever drives the run —
// importing that feature. A delegation's parent learns its child ended; a team
// learns a member did. Most sessions concern no observer, so an observer MUST
// be a no-op for a session it does not recognise.
type SettledObserver interface {
	OnSessionSettled(ctx context.Context, tenantID, sessionID uuid.UUID) error
}

// NotifySettled tells every registered observer that sessionID settled. A
// failing observer is logged and never stops the others or fails the caller:
// the run already happened, and an observer's bookkeeping is recoverable by its
// own sweep. Every path that drives a session (a queue worker, an approval
// resume) calls this one method, so a new such path cannot forget an observer.
func (c *Control) NotifySettled(ctx context.Context, tenantID, sessionID uuid.UUID) {
	for _, o := range c.Observers {
		if err := o.OnSessionSettled(ctx, tenantID, sessionID); err != nil {
			log.Error().Err(err).Any("tenant_id", tenantID).Any("session_id", sessionID).Msg("runctl: session-settled observer failed")
		}
	}
}
