// Package delegate implements the delegation transaction (README.md §8,
// Phase 8, tasks 8.8-8.14): spawning a child session from an ordinary
// `delegate` tool invocation, tracking it until the child returns, folding
// its taint into the parent (task 8.11), and resuming the parent's
// suspended kernel run with the outcome (task 8.10) — the SAME durable
// suspend shape internal/oversight already uses for approvals, driven by a
// DIFFERENT trigger (a child's own terminal state, never a human decision).
//
// delegate is free to import kernel and internal/tools directly — only
// kernel's own import allowlist is restricted (kernel/types.go); nothing
// stops a package the kernel doesn't import from importing the kernel, the
// same rule internal/oversight and internal/runctl's own package docs
// already state.
package delegate

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Status is the delegations.status column's vocabulary.
type Status string

const (
	StatusPending       Status = "pending"
	StatusReturned      Status = "returned"
	StatusReaped        Status = "reaped"
	StatusBoundExceeded Status = "bound_exceeded"
)

// Bounds README task 8.12 names — fail closed, no configuration widens them
// this phase (the same "no widening operation exists" discipline
// internal/permissions/autonomy.go's own ratchet already follows).
const (
	MaxDepth      = 1  // a session at Depth 1 (already a delegate's child) may not itself delegate
	MaxConcurrent = 3  // open (status=pending) delegations per root_session_id
	MaxPerRun     = 16 // delegations ever created (any status) per root_session_id
)

// Delegation mirrors one delegations row.
type Delegation struct {
	DelegationID         uuid.UUID
	TenantID             uuid.UUID
	ParentSessionID      uuid.UUID
	ChildSessionID       uuid.UUID
	ParentToolUseEventID uuid.UUID // uuid.Nil until Bind
	FanoutID             *uuid.UUID
	AgentID              string
	Task                 string
	ScopeGrant           []string
	ReturnSchema         json.RawMessage
	Status               Status
	Result               json.RawMessage
	Reason               string
	CreatedAt            time.Time
	ResolvedAt           *time.Time
}
