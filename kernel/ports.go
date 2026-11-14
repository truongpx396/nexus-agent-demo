package kernel

import (
	"context"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent/internal/eventlog"
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
	// source of truth. Use it for a change no event justifies (a run starting
	// or resuming); when an event justifies the change, use AppendAndSetStatus.
	SetStatus(ctx context.Context, tenantID, sessionID uuid.UUID, status string, terminalReason *string) error

	// AppendAndSetStatus is Append and SetStatus in one atomic operation: the
	// event and the status it justifies commit together or not at all. Done as
	// two calls, a crash between them leaves a terminal event on a session still
	// marked running, which orphan recovery then drives again, or a suspension
	// event on a session still marked running.
	AppendAndSetStatus(ctx context.Context, e eventlog.Event, status string, terminalReason *string) (eventlog.Event, error)

	// RecordSuspension durably records the approval the run just suspended on
	// (SuspendRequest), so an approval_requested event is never left without a
	// row to resolve it against. An adapter with nothing to record returns nil.
	RecordSuspension(ctx context.Context, req SuspendRequest) error

	// RecordDelegation durably binds the delegation a run just suspended on to
	// the tool_use that gates it (DelegateSuspendRequest). An adapter with
	// nothing to record returns nil.
	RecordDelegation(ctx context.Context, req DelegateSuspendRequest) error

	// AppendBatch appends events for one session in order, as one atomic
	// operation: every one is durable or none is, and the returned events carry
	// the Seq and CreatedAt the log assigned. It exists for a run's opening
	// events (one per resident tool, then the memory and the user's message):
	// written one at a time that is a transaction, an advisory lock, a receipt
	// and a signer round trip each, all before the caller has its answer.
	AppendBatch(ctx context.Context, events []eventlog.Event) ([]eventlog.Event, error)
}

// AutonomyReader is an optional part of an EventLog: it says what autonomy
// level the session is pinned at right now. RunConfig carries the level the
// run started with, and a run can last for hours; an operator who tightens a
// session mid-run (runctl.TightenAutonomy) changes the session row, so a run
// that never asks would keep acting under the looser level. The level only
// ever tightens, so reading it before each batch of tool calls is enough. A log
// without it leaves the run on its configured level.
type AutonomyReader interface {
	CurrentAutonomy(ctx context.Context, tenantID, sessionID uuid.UUID) (string, error)
}
