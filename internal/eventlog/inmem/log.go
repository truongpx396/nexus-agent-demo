// Package inmem is an in-memory kernel.EventLog: the adapter that lets the
// kernel loop run in a unit test with no database, no tenant scoping and no
// audit chain. It is a faithful implementation of the port's contract
// (internal/eventlog/logtest runs the same suite against it and against the
// Postgres adapter), not a stub: per-session sequence numbers, a stored
// status projection, and recorded suspensions and delegations.
//
// It is for tests and local experiments. Nothing here is durable, and it
// deliberately does no encryption or tenant isolation — those are properties
// of the production adapter, proven in tests/integration.
package inmem

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// Status is a session's status projection as last written by SetStatus.
type Status struct {
	Value          string
	TerminalReason *string
}

// Log is an in-memory kernel.EventLog, safe for concurrent use.
type Log struct {
	// FailAppend, if set, is consulted before every Append; a non-nil return
	// is the error Append reports and nothing is recorded. It exists so a test
	// can prove how the kernel reacts when the log fails mid-run.
	FailAppend func(eventlog.Event) error

	mu          sync.Mutex
	events      map[uuid.UUID][]eventlog.Event
	status      map[uuid.UUID]Status
	suspensions []kernel.SuspendRequest
	delegations []kernel.DelegateSuspendRequest
}

var _ kernel.EventLog = (*Log)(nil)

// New returns an empty Log.
func New() *Log {
	return &Log{events: map[uuid.UUID][]eventlog.Event{}, status: map[uuid.UUID]Status{}}
}

func (l *Log) Append(_ context.Context, e eventlog.Event) (eventlog.Event, error) {
	if l.FailAppend != nil {
		if err := l.FailAppend(e); err != nil {
			return eventlog.Event{}, err
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Seq = int64(len(l.events[e.SessionID])) + 1
	e.CreatedAt = time.Now().UTC()
	l.events[e.SessionID] = append(l.events[e.SessionID], e)
	return e, nil
}

func (l *Log) SetStatus(_ context.Context, _, sessionID uuid.UUID, status string, terminalReason *string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.status[sessionID] = Status{Value: status, TerminalReason: terminalReason}
	return nil
}

func (l *Log) RecordSuspension(_ context.Context, req kernel.SuspendRequest) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.suspensions = append(l.suspensions, req)
	return nil
}

func (l *Log) RecordDelegation(_ context.Context, req kernel.DelegateSuspendRequest) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.delegations = append(l.delegations, req)
	return nil
}

// Events returns a copy of sessionID's events in sequence order.
func (l *Log) Events(sessionID uuid.UUID) []eventlog.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]eventlog.Event(nil), l.events[sessionID]...)
}

// Status returns sessionID's status projection; ok is false if SetStatus has
// never been called for it.
func (l *Log) Status(sessionID uuid.UUID) (s Status, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok = l.status[sessionID]
	return s, ok
}

// Suspensions returns a copy of every RecordSuspension request, in order.
func (l *Log) Suspensions() []kernel.SuspendRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]kernel.SuspendRequest(nil), l.suspensions...)
}

// Delegations returns a copy of every RecordDelegation request, in order.
func (l *Log) Delegations() []kernel.DelegateSuspendRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]kernel.DelegateSuspendRequest(nil), l.delegations...)
}
