// Package postgres is the production adapter for kernel.EventLog: it persists
// the append-only event log in Postgres through internal/store, one
// tenant-scoped transaction per port call (docs/constitution.md, Principle
// VI — scoping is transaction-local, never session-level).
//
// What must commit atomically with an event is this adapter's configuration,
// not the kernel's. The audit chain extends in the same transaction as the event
// it covers, so an event is never observable without a receipt; that is Hooks.
// Chain, held here because it is persistence (internal/audit/postgres), not
// something a caller can run in its own transaction. The approval row and the
// delegation bind a suspension records are different: the kernel asks for them
// after the event is appended, each a use case of its own feature in its own
// transaction, so Hooks.Suspend and Hooks.Delegate are plain callbacks over the
// kernel's request types and mention no transaction. cmd/nexusd is the only
// place that wires real ones in; a zero Hooks is valid and means "that control
// isn't wired", which is how a minimal log is built in tests.
package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditpg "github.com/truongpx396/nexus-agent-demo/internal/audit/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// SuspendFunc durably records the approval a run just suspended on
// (oversight.Approvals.Create), so an approval_requested event is never left
// without a row to resolve it against. It owns its own transaction.
type SuspendFunc func(ctx context.Context, req kernel.SuspendRequest) error

// DelegateFunc durably binds a delegation to the tool_use it gates
// (delegate.Delegations.BindToolUse). It owns its own transaction.
type DelegateFunc func(ctx context.Context, req kernel.DelegateSuspendRequest) error

// Hooks are the collaborators of a Log. Every field is optional; nil means that
// control isn't wired.
type Hooks struct {
	// Chain extends the tenant's hash-chained audit log for each event the log
	// appends, in the same transaction as the append.
	Chain *auditpg.Chain
	// Suspend records the approval a run suspended on.
	Suspend SuspendFunc
	// Delegate binds a delegation to its gating tool_use.
	Delegate DelegateFunc
}

// Log is the Postgres kernel.EventLog.
type Log struct {
	store *store.Store
	hooks Hooks
}

var _ kernel.EventLog = (*Log)(nil)

// New returns a Log over st with the given collaborators.
func New(st *store.Store, hooks Hooks) *Log {
	return &Log{store: st, hooks: hooks}
}

// Append inserts e and, when a chain is wired, extends the audit chain for it in
// the same transaction.
func (l *Log) Append(ctx context.Context, e eventlog.Event) (eventlog.Event, error) {
	var out eventlog.Event
	err := l.store.InTenantTx(ctx, e.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var aerr error
		out, aerr = store.Append(ctx, tx, e)
		if aerr != nil {
			return aerr
		}
		if l.hooks.Chain != nil {
			_, cerr := l.hooks.Chain.Append(ctx, tx, out.TenantID, out.SessionID, out.Seq, out.EventID, string(out.Type), out.PayloadDigest)
			return cerr
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

// RecordSuspension runs the approval hook, or does nothing when none is wired.
func (l *Log) RecordSuspension(ctx context.Context, req kernel.SuspendRequest) error {
	if l.hooks.Suspend == nil {
		return nil
	}
	return l.hooks.Suspend(ctx, req)
}

// RecordDelegation runs the delegation hook, or does nothing when none is wired.
func (l *Log) RecordDelegation(ctx context.Context, req kernel.DelegateSuspendRequest) error {
	if l.hooks.Delegate == nil {
		return nil
	}
	return l.hooks.Delegate(ctx, req)
}

// AppendBatch appends every event, and extends the audit chain for each, in one
// transaction. The events in a batch belong to one tenant; they are applied in the
// order given, so their receipts chain in that order.
func (l *Log) AppendBatch(ctx context.Context, events []eventlog.Event) ([]eventlog.Event, error) {
	if len(events) == 0 {
		return nil, nil
	}
	out := make([]eventlog.Event, 0, len(events))
	err := l.store.InTenantTx(ctx, events[0].TenantID, func(ctx context.Context, tx pgx.Tx) error {
		out = out[:0]
		for _, e := range events {
			if e.TenantID != events[0].TenantID {
				return fmt.Errorf("append batch: events span tenants %s and %s", events[0].TenantID, e.TenantID)
			}
			appended, aerr := store.Append(ctx, tx, e)
			if aerr != nil {
				return aerr
			}
			if l.hooks.Chain != nil {
				if _, cerr := l.hooks.Chain.Append(ctx, tx, appended.TenantID, appended.SessionID, appended.Seq, appended.EventID, string(appended.Type), appended.PayloadDigest); cerr != nil {
					return cerr
				}
			}
			out = append(out, appended)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
