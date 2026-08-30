// Package capability is the shared vocabulary every surface declares itself
// against (README task 7.12, pattern #49): a static Descriptor (what a
// surface CAN do — never per-request) and a per-request Principal (WHO is
// submitting THIS turn — always per-request, task 7.13). Two surfaces
// today, internal/surfaces/rest and internal/surfaces/cli, each declare
// their own package-level Descriptor value; Phase 11's additional surfaces
// (MCP, Telegram, Zalo, email, cron, web) declare theirs the same way.
package capability

import "github.com/google/uuid"

// PrincipalKind names what kind of caller submitted one turn — "user" for
// every surface this phase ships; "scheduler" is declared now as the
// value Phase 11's cron surface will need, the same forward-reference
// idiom this codebase already uses for schema columns it doesn't populate
// yet (e.g. sessions.region).
type PrincipalKind string

const (
	PrincipalUser      PrincipalKind = "user"
	PrincipalScheduler PrincipalKind = "scheduler"
)

// Principal is the turn-submitting identity (task 7.13) — resolved fresh
// from the inbound request/turn every time, never cached from whoever
// opened the conversation or a prior turn on the same session. A surface
// resolves one of these per request; it is never stored and reused.
type Principal struct {
	Kind     PrincipalKind
	TenantID uuid.UUID
	UserID   uuid.UUID
}

// Descriptor is what one surface declares about itself, once, statically —
// never per-request (task 7.12). Approval routing and any other
// capability-gated code path filter on this rather than special-casing a
// surface by name.
type Descriptor struct {
	SurfaceID     string
	PrincipalKind PrincipalKind

	// CanRenderApprovalContext: this surface can show a human the
	// decision-ready context an approval carries (tool_id, effect_class,
	// input) — not just a bare identifier. A surface that can't (a minimal
	// webhook ack, say) gets RenderApprovalContext's one-line fallback
	// instead of the full breakdown.
	CanRenderApprovalContext bool
	// SupportsStepUp: this surface can prompt for a second factor/
	// elevated confirmation inline, for an approval whose policy demands
	// one (README pattern #23's approval_policy multi_party/step-up
	// asks). Declared now as a seam; no surface in this phase actually
	// enforces a step-up requirement yet.
	SupportsStepUp bool
	// SupportsStructuredInput: this surface can accept a structured
	// (JSON) reply, not only free text — gates whether a schema-declared
	// input_request (README pattern #24) can be answered directly versus
	// needing a free-text parse.
	SupportsStructuredInput bool
	// SupportsStreaming: this surface can deliver a run's events as they
	// happen (SSE, a long-poll, ...) rather than only a final result.
	SupportsStreaming bool
}
