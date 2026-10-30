package eventlog

import (
	"context"

	"github.com/google/uuid"
)

// Appender appends one system-authored event to a session's log from inside a
// unit of work that is already open: the payload is marshalled, sealed under
// the session's own active key, written to the log, and extended into the
// audit chain, all in the caller's transaction. That is how a state change and
// the event that records it commit together — an approval is granted if and
// only if its approval_granted event exists — for the services (oversight,
// runctl, delegation, teams, plans, grants, the delivery outbox) that act on a
// session out of band, with no live kernel run to seal for them.
//
// An implementation is bound to one unit of work (the Postgres one to a
// pgx.Tx), which is why Append takes no transaction. The kernel does not use
// it: it appends through kernel.EventLog, with the run's own seal function.
type Appender interface {
	Append(ctx context.Context, tenantID, sessionID uuid.UUID, typ EventType, toolID *string, pairRef *uuid.UUID, payload any) (Event, error)
}
