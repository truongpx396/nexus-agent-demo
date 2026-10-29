// Package postgres is the production adapter for kernel.EventLog: it persists
// the append-only event log in Postgres through internal/store, one
// tenant-scoped transaction per port call (docs/constitution.md, Principle
// VI — scoping is transaction-local, never session-level).
//
// What must commit atomically with an event is this adapter's configuration,
// not the kernel's: Hooks carries the audit receipt that extends the
// hash-chained audit log in the same transaction as the event it covers, and
// the approval and delegation rows a suspension binds to. cmd/nexusd is the
// only place that wires real ones in; a zero Hooks is valid and means "that
// control isn't wired", which is how a minimal log is built in tests.
package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// ReceiptFunc extends the tenant's hash-chained audit receipt
// (internal/audit.Chain.Append) for one event the log just appended. It runs
// inside the same transaction as the append, so an event is never observable
// without a receipt for it.
type ReceiptFunc func(ctx context.Context, tx pgx.Tx, e eventlog.Event) error

// OnSuspend durably records the approval a run just suspended on
// (internal/oversight.Approvals.Create), so an approval_requested event is
// never left without a row to resolve it against.
type OnSuspend func(ctx context.Context, tx pgx.Tx, req kernel.SuspendRequest) error

// OnDelegate durably binds a delegation to the tool_use it gates
// (internal/delegate.Delegations.Bind).
type OnDelegate func(ctx context.Context, tx pgx.Tx, req kernel.DelegateSuspendRequest) error

// Hooks are the same-transaction side effects of a Log. Every field is
// optional; nil means that control isn't wired.
type Hooks struct {
	Receipt  ReceiptFunc
	Suspend  OnSuspend
	Delegate OnDelegate
}

// Log is the Postgres kernel.EventLog.
type Log struct {
	store *store.Store
	hooks Hooks
}

var _ kernel.EventLog = (*Log)(nil)

// New returns a Log over st with the given same-transaction hooks.
func New(st *store.Store, hooks Hooks) *Log {
	return &Log{store: st, hooks: hooks}
}

// Append inserts e and, when a receipt hook is wired, extends the audit chain
// for it in the same transaction.
func (l *Log) Append(ctx context.Context, e eventlog.Event) (eventlog.Event, error) {
	var out eventlog.Event
	err := l.store.InTenantTx(ctx, e.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var aerr error
		out, aerr = store.Append(ctx, tx, e)
		if aerr != nil {
			return aerr
		}
		if l.hooks.Receipt != nil {
			return l.hooks.Receipt(ctx, tx, out)
		}
		return nil
	})
	if err != nil {
		return eventlog.Event{}, err
	}
	return out, nil
}

// SetStatus writes the session's status projection.
func (l *Log) SetStatus(ctx context.Context, tenantID, sessionID uuid.UUID, status string, terminalReason *string) error {
	return l.store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.UpdateSessionStatus(ctx, tx, sessionID, status, terminalReason)
	})
}

// RecordSuspension runs the approval hook in its own transaction, or does
// nothing when none is wired.
func (l *Log) RecordSuspension(ctx context.Context, req kernel.SuspendRequest) error {
	if l.hooks.Suspend == nil {
		return nil
	}
	return l.store.InTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return l.hooks.Suspend(ctx, tx, req)
	})
}

// RecordDelegation runs the delegation hook in its own transaction, or does
// nothing when none is wired.
func (l *Log) RecordDelegation(ctx context.Context, req kernel.DelegateSuspendRequest) error {
	if l.hooks.Delegate == nil {
		return nil
	}
	return l.store.InTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return l.hooks.Delegate(ctx, tx, req)
	})
}
