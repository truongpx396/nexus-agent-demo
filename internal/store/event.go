// Package store owns the append-only event log: the single source of truth
// (docs/constitution.md, Principle II). Every other mutable column anywhere
// in this system is a projection rebuildable by replaying this log — never
// an independent source of truth.
package store

import (
	"time"

	"github.com/google/uuid"
)

// Actor identifies who produced an event.
type Actor string

const (
	ActorModel  Actor = "model"
	ActorTool   Actor = "tool"
	ActorUser   Actor = "user"
	ActorSystem Actor = "system"
)

// EventType is the taxonomy an append-only log replays through. It must stay
// complete enough to reconstruct any run — including who authorized what, on
// what basis, and when — from the log alone (README.md §5, task 1.5).
type EventType string

const (
	// Model output
	EventThought EventType = "thought"
	EventContent EventType = "content"
	EventToolUse EventType = "tool_use"

	// Tool
	EventToolResult          EventType = "tool_result"
	EventToolReceiptRef      EventType = "tool_receipt_ref"
	EventEffectClaimed       EventType = "effect_claimed"
	EventEffectClaimResolved EventType = "effect_claim_resolved"

	// Context
	EventCondensation  EventType = "condensation"
	EventContextPruned EventType = "context_pruned"
	EventCheckpoint    EventType = "checkpoint"

	// Human (push)
	EventUserMessage EventType = "user_message"

	// Human (pull)
	EventInputRequested   EventType = "input_requested"
	EventInputAnswered    EventType = "input_answered"
	EventInputExpired     EventType = "input_expired"
	EventInputInvalidated EventType = "input_invalidated"

	// Approval
	EventApprovalRequested         EventType = "approval_requested"
	EventApprovalNotified          EventType = "approval_notified"
	EventApprovalReminded          EventType = "approval_reminded"
	EventApprovalEscalated         EventType = "approval_escalated"
	EventApprovalGranted           EventType = "approval_granted"
	EventApprovalGrantedModified   EventType = "approval_granted_modified"
	EventApprovalDenied            EventType = "approval_denied"
	EventApprovalExpired           EventType = "approval_expired"
	EventApprovalInvalidated       EventType = "approval_invalidated"
	EventApprovalResolutionRefused EventType = "approval_resolution_refused"
	EventApprovalMismatch          EventType = "approval_mismatch"

	// Cost & budget
	EventBudgetDecision EventType = "budget_decision"

	// Catalog
	EventToolLoaded EventType = "tool_loaded"

	// Memory
	EventMemoryLoaded EventType = "memory_loaded"

	// Skills
	EventSkillActivated         EventType = "skill_activated"
	EventSkillCapabilityIgnored EventType = "skill_capability_ignored"

	// Delivery
	EventDeliveryEnqueued   EventType = "delivery_enqueued"
	EventDeliveryDelivered  EventType = "delivery_delivered"
	EventDeliveryFailed     EventType = "delivery_failed"
	EventDeliverySuppressed EventType = "delivery_suppressed"

	// Safety
	EventTaintTransition      EventType = "taint_transition"
	EventSanitizationBoundary EventType = "sanitization_boundary"

	// Delegation
	EventDelegationRequested      EventType = "delegation_requested"
	EventDelegationTargetSelected EventType = "delegation_target_selected"
	EventDelegationRefused        EventType = "delegation_refused"
	EventDelegationReturned       EventType = "delegation_returned"
	EventDelegationReaped         EventType = "delegation_reaped"

	// Orchestration
	EventPlanStarted     EventType = "plan_started"
	EventPlanStepEntered EventType = "plan_step_entered"
	EventPlanTransition  EventType = "plan_transition"
	EventPlanStepExited  EventType = "plan_step_exited"
	EventPlanCompleted   EventType = "plan_completed"

	// Observability
	EventContentAccessGranted EventType = "content_access_granted"
	EventContentAccessed      EventType = "content_accessed"
	EventContentAccessRefused EventType = "content_access_refused"

	// Lifecycle
	EventError          EventType = "error"
	EventStuckSuspected EventType = "stuck_suspected"
	EventForked         EventType = "forked"
	EventTerminal       EventType = "terminal"
	EventErasure        EventType = "erasure"
)

// CurrentSchemaVersion is the envelope version new events are written under.
// A documented Upcaster path (upcast.go) keeps events written under an older
// version replayable after a schema change (FR-086).
const CurrentSchemaVersion = 2

// Event is a typed, timestamped, attributable record appended to the log —
// the single source of truth (FR-002, FR-003, FR-040).
type Event struct {
	EventID       uuid.UUID
	SessionID     uuid.UUID
	TenantID      uuid.UUID
	Seq           int64
	SchemaVersion int
	Type          EventType
	Payload       []byte // ciphertext under the tenant/subject key (internal/crypto)
	PayloadDigest []byte // digest over PLAINTEXT — survives crypto-shredding
	KeyID         string // which key sealed Payload; destroying it is erasure
	Actor         Actor
	ToolID        *string
	PairRef       *uuid.UUID // links a tool_result to its tool_use (FR-003)
	ModelID       *string
	TraceID       []byte
	SpanID        []byte
	CreatedAt     time.Time
}
