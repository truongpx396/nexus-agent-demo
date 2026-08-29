// Package tools is the tool pipeline (README task 3.4, pattern 16): the
// single execution path every tool_use is dispatched through, from
// identity resolution to result budgeting (pipeline.go's 16 ordered steps).
// It is also the one place internal/permissions and internal/hooks are
// wired together for a real invocation — both of those packages stay leaves
// with respect to this one.
package tools

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// EffectClass classifies what a tool DOES, independent of what it happens
// to be named — the input to layer 3 (autonomy) and layer 8 (approval
// policy) of the permission chain.
type EffectClass string

const (
	EffectClassReadOnly EffectClass = "read_only"
	EffectClassMutating EffectClass = "mutating"
	EffectClassExternal EffectClass = "external"
)

// Taint declares what a tool's OUTPUT and EXECUTION touch — every field
// defaults to TRUE (DefaultTaint) so an under-declared tool fails closed
// into the Rule of Two's strictest reading rather than a permissive one
// (README.md §4's kernel-ABI comment: "every field of Taint defaults to
// TRUE").
type Taint struct {
	ReturnsUntrusted bool // output must be treated as untrusted content, never as instructions
	ReadsPrivateData bool
	MutatesExternal  bool
}

// DefaultTaint is the fail-closed default a tool gets if it declares
// nothing narrower.
func DefaultTaint() Taint {
	return Taint{ReturnsUntrusted: true, ReadsPrivateData: true, MutatesExternal: true}
}

// Descriptor is a tool's admitted, catalog-resident identity: what a
// manifest pins, what the admission scanner scans, and what the digest in
// harness_digest is computed over.
type Descriptor struct {
	ID          ToolRef
	Description string
	InputSchema json.RawMessage
	EffectClass EffectClass
}

// PermissionResult is a tool's own opinion — Gate 2, capability metadata
// (README.md §4's chain table, row 5). Decision must be Deny, Ask, or
// Defer; Allow is refused at the chain (internal/permissions.Chain.Resolve
// rejects a precomputed Gate 2 outcome of Allow as a bug, never silently
// accepts it).
type PermissionResult struct {
	Decision string // "deny" | "ask" | "defer"
	Reason   string
}

// Result is a tool's raw return from Call.
type Result struct {
	Output  json.RawMessage
	IsError bool
	Reason  string
}

// RunContext carries the identifiers and per-session facilities a tool call
// needs. WorkspaceDir is the per-session directory builtin filesystem tools
// are scoped to — there is no sandbox yet (that's Phase 5's
// internal/sandbox); this is the seam it will replace, not a security
// boundary itself.
type RunContext struct {
	TenantID     uuid.UUID
	SessionID    uuid.UUID
	WorkspaceDir string
}

// Tool is the kernel ABI's Tool interface (README.md §4), translated
// verbatim: identity, self-description, taint, concurrency safety, the
// tool's own permission opinion, input validation, and execution.
type Tool interface {
	ID() ToolRef
	Descriptor() Descriptor
	Taint() Taint
	IsConcurrencySafe(input json.RawMessage) bool
	CheckPermissions(ctx context.Context, in json.RawMessage, rc RunContext) PermissionResult
	ValidateInput(ctx context.Context, in json.RawMessage, rc RunContext) error
	Call(ctx context.Context, in json.RawMessage, rc RunContext) (Result, error)
}
