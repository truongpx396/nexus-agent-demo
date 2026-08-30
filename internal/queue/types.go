// Package queue is the durable job queue README task 6.1 names: a Postgres
// table polled with SELECT ... FOR UPDATE SKIP LOCKED, plus a Redis
// session-key serial lock (task 6.2) and a worker pool that pulls jobs and
// runs them. It carries only asynchronous, session-scoped CONTROL work —
// resuming a session after a crash, forking one, or draining a steer — never
// a fresh interactive run, which stays on internal/surfaces/rest's existing
// synchronous fast path: minting a session and its DEK, and starting the
// first turn, all happen inline with the HTTP request that asked for them,
// exactly as Phase 2 shipped it. Queuing THAT would mean carrying a user's
// plaintext opening message through queue_jobs.payload, which (unlike
// events.payload) has no sealed-envelope column to protect it — an honest
// scope line, not an oversight.
//
// This package stays free of any kernel/tools/store/crypto dependency —
// Runner is the seam a caller (cmd/nexusd) plugs the actual work into,
// mirroring kernel.ToolExecutor's own decoupling idiom.
package queue

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Kind is queue_jobs.kind's vocabulary.
type Kind string

const (
	KindResume Kind = "resume"
	KindFork   Kind = "fork"
	KindSteer  Kind = "steer"
)

// Status is queue_jobs.status's vocabulary.
type Status string

const (
	StatusPending Status = "pending"
	StatusLeased  Status = "leased"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

// Job mirrors one queue_jobs row.
type Job struct {
	JobID          uuid.UUID
	TenantID       uuid.UUID
	SessionID      uuid.UUID
	SessionKey     string
	Kind           Kind
	Payload        json.RawMessage
	Status         Status
	Attempts       int
	AvailableAt    time.Time
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	LastError      string
	CreatedAt      time.Time
}

// Port is the queue's own abstract interface (mirrors internal/cost.
// BudgetGate / kernel.ToolExecutor's own decoupling idiom): Postgres
// (postgres.go) is the only adapter this demo ships — NATS JetStream is
// deferred (README §2's infrastructure collapse) — but every call site
// depends on this interface, never *Postgres directly, so a future adapter
// is a `main.go` change, not a rewrite.
type Port interface {
	// Enqueue durably records a new job, pending immediately (or at a
	// caller-specified future AvailableAt, for a deliberately delayed
	// retry).
	Enqueue(ctx context.Context, job Job) (Job, error)
	// Lease atomically claims one leasable job (status=pending,
	// available_at <= now) for owner, marking it leased with a lease that
	// expires after leaseFor — SKIP LOCKED under the hood so N workers
	// polling concurrently never double-lease the same row. ok is false
	// when nothing is currently leasable.
	Lease(ctx context.Context, owner string, leaseFor time.Duration) (Job, bool, error)
	// Complete marks jobID done.
	Complete(ctx context.Context, jobID uuid.UUID) error
	// Fail records jobID's failure and either requeues it (status back to
	// pending, available_at = retryAt) for another attempt, or marks it
	// permanently failed if permanent is true — task 6.7's typed
	// classification is what decides which.
	Fail(ctx context.Context, jobID uuid.UUID, reason string, permanent bool, retryAt time.Time) error
}
