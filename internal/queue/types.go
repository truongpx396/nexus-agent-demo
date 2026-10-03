// Package queue is the durable job queue README task 6.1 names: a Redis
// Stream + consumer group (redis_streams.go — the retired Postgres SKIP
// LOCKED adapter's successor; NATS JetStream remains the deferred,
// real-production option), plus a Redis session-key serial lock (task 6.2)
// and a worker pool that pulls jobs and runs them. Every mutation that
// drives kernel.Kernel's turn loop goes through here now — a fresh
// interactive run's Kernel.Seed step (cmd/nexusd's kernelRunStarter) and a
// conversational follow-up's message-append (internal/runctl.Control.
// ResumeConversation) both durably append their own sealed event to
// Postgres FIRST, then enqueue a job carrying only ids — never a plaintext
// opening message through a stream entry, which (unlike events.payload) has
// no sealed-envelope column to protect it. Fork/Cancel/TightenAutonomy/Steer
// stay on internal/surfaces/rest's synchronous fast path: none of them
// drive the turn loop themselves, so queuing them would add a hop for no
// scalability gain.
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

// Kind is one queued job's vocabulary — purely for log/metric
// observability (queueRunner.Run, cmd/nexusd/background.go): every Kind
// dispatches to the identical runctl.Control.Resume call, since by the time
// a worker picks any of them up, a fresh-started, crash-recovered, and
// human-followed-up session all look the same — their durable seed/message
// event is already in the Postgres event log.
type Kind string

const (
	KindResume   Kind = "resume"   // a crash/orphan sweep re-entering an existing run
	KindStart    Kind = "start"    // a fresh run, seeded (kernel.Kernel.Seed) synchronously before this was enqueued
	KindConverse Kind = "converse" // a conversational session's human follow-up message, appended before this was enqueued
)

// Status is one job's own lifecycle vocabulary — StatusDone/StatusFailed
// exist for Port implementations that keep a queryable record after a job
// leaves the live queue (RedisStreams' own dead-letter stream, historically
// queue_jobs.status='failed' rows); Enqueue/Lease/Complete/Fail's return
// values only ever use Pending/Leased.
type Status string

const (
	StatusPending Status = "pending"
	StatusLeased  Status = "leased"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

// Job is one unit of queued work — historically one queue_jobs row, now one
// Redis Stream entry (redis_streams.go).
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
// BudgetGate / kernel.ToolExecutor's own decoupling idiom): RedisStreams
// (redis_streams.go) is the demo's own adapter — NATS JetStream remains
// deferred (README §2's infrastructure collapse) — but every call site
// depends on this interface, never *RedisStreams directly, so a future
// adapter is a `main.go` change, not a rewrite. This interface's shape
// (poll-once, one attempt per call, ok bool) predates Redis Streams — it
// was designed for Postgres SKIP LOCKED polling — and is kept unchanged
// rather than redesigned around XREADGROUP's own blocking-read model,
// since Worker's admission/breaker/session-lock sequencing (worker.go) and
// its tests are all written against exactly this shape.
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
