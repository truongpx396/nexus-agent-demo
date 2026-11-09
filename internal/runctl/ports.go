package runctl

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// ClaimStatus is the claims table's status vocabulary (migrations/
// 0012_claims.sql, README task 6.6).
type ClaimStatus string

const (
	ClaimInFlight  ClaimStatus = "in_flight"
	ClaimCompleted ClaimStatus = "completed"
	ClaimAbandoned ClaimStatus = "abandoned"
)

// Claim mirrors one claims row: a write-ahead record that a non-read-only
// tool's effect is (or was) about to leave the process, keyed by
// (session_id, canonical_digest) — the same digest internal/tools' task 3.5
// already names as an idempotency key. It carries no tool output and no notion
// of "the effect succeeded" beyond its own three-state Status — deliberately:
// the ONLY authoritative record of what a tool actually returned is the event
// log's own tool_result, never this row.
type Claim struct {
	ClaimID         uuid.UUID
	TenantID        uuid.UUID
	SessionID       uuid.UUID
	ToolID          string
	CanonicalDigest []byte
	Status          ClaimStatus
	Reason          string
	CreatedAt       time.Time
	ResolvedAt      *time.Time
}

// Tx is one open unit of work in a single tenant's scope: everything cancel,
// steer, resume, fork, replay and the claim ledger read and change, so that a
// session's status, its claims and the event that records each change commit
// together or not at all. The Postgres implementation is
// internal/runctl/postgres, a pgx transaction under the tenant's row-level
// security; what each operation may do, and in which states, is this package's.
type Tx interface {
	eventlog.Appender
	eventlog.SessionRows
	crypto.Keys

	// OpenOrFindClaim durably records a fresh in_flight claim for
	// (sessionID, digest) if none exists (created=true), or returns the
	// existing row unchanged (created=false). The (session_id,
	// canonical_digest) uniqueness is what makes this atomic under concurrent
	// callers.
	OpenOrFindClaim(ctx context.Context, tenantID, sessionID uuid.UUID, toolID string, digest []byte) (claim Claim, created bool, err error)

	// ResolveClaim moves a claim to status with reason.
	ResolveClaim(ctx context.Context, claimID uuid.UUID, status ClaimStatus, reason string) (Claim, error)

	// ListInFlightClaims is the session's claims no one has resolved.
	ListInFlightClaims(ctx context.Context, sessionID uuid.UUID) ([]Claim, error)

	// ClaimAwaitingInput atomically moves a session from awaiting_input back to
	// running and reports whether this caller was the one that did: of N
	// concurrent callers exactly one gets true.
	ClaimAwaitingInput(ctx context.Context, sessionID uuid.UUID) (bool, error)

	// RecordFork records that sessionID is a fork of parentSessionID taken at
	// event atSeq, with what the fork changed. The record is runctl's own: the
	// session aggregate does not carry fork provenance.
	RecordFork(ctx context.Context, tenantID, sessionID, parentSessionID uuid.UUID, atSeq int64, overrides map[string]string) error

	// SetAutonomyLevel pins the session's autonomy level.
	SetAutonomyLevel(ctx context.Context, sessionID uuid.UUID, level string) error

	// SaveCheckpoint appends a checkpoint row (README task 6.3) and returns it
	// with its id and creation time filled in.
	SaveCheckpoint(ctx context.Context, c Checkpoint) (Checkpoint, error)

	// LatestCheckpoint returns the session's most recent checkpoint, if any. No
	// runtime path reads it yet: it is the recovery fast path a later resume
	// will consult, kept here so the port is the whole of the table's contract.
	LatestCheckpoint(ctx context.Context, sessionID uuid.UUID) (Checkpoint, bool, error)

	// ListStaleAwaitingInput returns the sessions that have sat in
	// awaiting_input since before olderThan (the idle-conversation sweep's query).
	ListStaleAwaitingInput(ctx context.Context, olderThan time.Time) ([]uuid.UUID, error)
}

// Store runs a unit of work in a tenant's scope: it opens the unit, calls fn
// with a Tx bound to it, and commits if fn returns nil or rolls back if not.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx Tx) error) error
}
