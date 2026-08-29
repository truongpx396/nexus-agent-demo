// Package oversight implements the approval and input-request transactions
// (README.md §5, Phase 5, tasks 5.6-5.10): everything that turns the
// EventApprovalRequested/EventInputRequested a kernel run's suspend point
// appends (kernel/loop.go's suspendForApproval, Phase 3) into a real human
// decision, and resumes the ONE pending tool call that decision was about
// (kernel.Kernel.Resume, README task 5.8). Approval and input-request share
// a lifecycle shape (pending -> decided/expired/invalidated) and nothing
// else: an approval's Grant authorizes an execution; an input's Answer
// never does (task 5.9's "carries zero authorization value").
//
// oversight is free to import kernel and internal/audit directly — only
// kernel's own import allowlist is restricted (kernel/types.go); nothing
// stops a package the kernel doesn't import from importing the kernel.
package oversight

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ApprovalStatus is the approvals.status column's vocabulary (migrations/
// 0007_oversight.sql).
type ApprovalStatus string

const (
	ApprovalPending         ApprovalStatus = "pending"
	ApprovalGranted         ApprovalStatus = "granted"
	ApprovalGrantedModified ApprovalStatus = "granted_modified"
	ApprovalDenied          ApprovalStatus = "denied"
	ApprovalExpired         ApprovalStatus = "expired"
	ApprovalInvalidated     ApprovalStatus = "invalidated"
)

// DefaultApprovalTTL is how long a pending approval stands before Expire
// (lazily, on decide, or via a sweep) resolves it as expired. Overridable
// per-Approvals instance; no per-tenant policy store exists yet (Phase 7).
const DefaultApprovalTTL = 24 * time.Hour

// ContextPackage is the decision-ready rendering an approver sees — never a
// bare UUID (README §5's Phase 5 demo text, verbatim: "renders recipient/
// subject/attachment digests ... never a bare UUID"). This demo's builtin
// tools (platform/file_write, platform/shell, platform/web_fetch, ...) have
// no per-field redaction metadata of their own, so the generic, honest
// rendering is the tool's identity, its effect class, and its ORIGINAL
// decoded input — an approver reading Input sees exactly the fields the
// call will execute with, not an opaque reference number.
type ContextPackage struct {
	ToolID      string          `json:"tool_id"`
	EffectClass string          `json:"effect_class,omitempty"`
	Input       json.RawMessage `json:"input"`
}

// Approval mirrors one approvals row.
type Approval struct {
	ApprovalID      uuid.UUID
	TenantID        uuid.UUID
	SessionID       uuid.UUID
	ToolUseEventID  uuid.UUID
	ToolID          string
	AskKind         string
	CanonicalDigest []byte
	Context         ContextPackage
	Assignee        string
	Status          ApprovalStatus
	GrantedInput    json.RawMessage
	ExpiresAt       time.Time
	DecidedAt       *time.Time
	DecidedBy       string
	Reason          string
	CreatedAt       time.Time
}

// InputStatus is the input_requests.status column's vocabulary.
type InputStatus string

const (
	InputPending     InputStatus = "pending"
	InputAnswered    InputStatus = "answered"
	InputExpired     InputStatus = "expired"
	InputInvalidated InputStatus = "invalidated"
)

// OnExpiry is input_requests.on_expiry's vocabulary (README task 5.9).
type OnExpiry string

const (
	OnExpiryExpire  OnExpiry = "expire"  // no answer arrives -> InputExpired, EventInputExpired
	OnExpiryDefault OnExpiry = "default" // no answer arrives -> the recorded DefaultAssumption, EventInputAnswered(used_default=true)
)

// InputRequest mirrors one input_requests row.
type InputRequest struct {
	InputRequestID    uuid.UUID
	TenantID          uuid.UUID
	SessionID         uuid.UUID
	Question          string
	Schema            json.RawMessage
	OnExpiry          OnExpiry
	DefaultAssumption json.RawMessage
	Status            InputStatus
	Answer            json.RawMessage
	UsedDefault       bool
	ExpiresAt         time.Time
	DecidedAt         *time.Time
	Reason            string
	CreatedAt         time.Time
}
