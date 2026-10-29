package kernel

import (
	"context"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// EventLog is the kernel's driven port onto durable conversation state: the
// only way the loop records anything. The kernel states what must become
// durable; the adapter decides how, and what else must commit atomically with
// it. internal/eventlog/postgres is the production adapter (one tenant-scoped
// transaction per call, so the audit receipt and the approval or delegation
// row commit with the event they belong to) and internal/eventlog/inmem is
// the in-memory one that lets the loop run in a unit test with no database.
// internal/eventlog/logtest holds the contract suite both must pass.
//
// Every method is one atomic operation: it commits in full or returns an
// error and leaves nothing behind. In particular an event is never observable
// without whatever the adapter was configured to record alongside it.
type EventLog interface {
	// Append durably records e and returns it with the Seq and CreatedAt the
	// log assigned (e's own values for both are ignored). Seq is the next
	// number in e.SessionID's own sequence, starting at 1.
	Append(ctx context.Context, e eventlog.Event) (eventlog.Event, error)

	// SetStatus writes the session's status, and its terminal reason once the
	// run has one. The session row is a projection of the log, never a second
	// source of truth; the kernel calls this right after appending the event
	// that justifies the change.
	SetStatus(ctx context.Context, tenantID, sessionID uuid.UUID, status string, terminalReason *string) error

	// RecordSuspension durably records the approval the run just suspended on
	// (SuspendRequest), so an approval_requested event is never left without a
	// row to resolve it against. An adapter with nothing to record returns nil.
	RecordSuspension(ctx context.Context, req SuspendRequest) error

	// RecordDelegation durably binds the delegation a run just suspended on to
	// the tool_use that gates it (DelegateSuspendRequest). An adapter with
	// nothing to record returns nil.
	RecordDelegation(ctx context.Context, req DelegateSuspendRequest) error
}
