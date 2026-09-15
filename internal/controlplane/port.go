// Package controlplane is the versioned control-plane/data-plane boundary
// README.md §2 has always described (README task 13.15, closing
// production-readiness finding F15: this package's stated deliverable —
// "interface + v1 shapes + import-boundary test" — didn't exist). Port is
// what a genuinely separate control-plane process would expose over RPC
// later; LocalPort (local.go) is the one implementation this single-binary
// demo needs today, composing the real internal/{cost,audit,oversight,obs}
// services directly. "One process today, two later" means splitting is a
// matter of replacing LocalPort with an RPC client behind this same
// interface — not a kernel change, and not a v1 shape change.
//
// Every v1 type here is deliberately plain data (uuid.UUID, string,
// []byte, time.Time, json.RawMessage) rather than a richer domain type
// from the packages Port's implementations wrap — the whole point of a
// versioned wire contract is that it doesn't change shape just because an
// internal type does. tests/contract/boundaries_test.go's own rule for
// this package (never import internal/{sandbox,memory,provider} — the
// data-plane-only packages) is what keeps that honest: DEK/encryption
// (internal/crypto) stays a data-plane concern entirely outside this
// boundary, on purpose, for the same reason — see AdmitRun's own doc
// comment.
package controlplane

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Port is the versioned control-plane contract (v1) — six calls, each its
// own atomic operation. Unlike an ambient shared transaction (impossible
// once this is a real RPC boundary between two processes), each method
// commits or fails independently; a caller sequencing several of them
// (AdmitRun then a first ReserveBudget, say) accepts that a failure between
// two independent calls leaves a bounded, inert partial state — never a
// security or double-spend hazard, because nothing here is a partial
// effect: an admitted-but-unresourced session simply never accumulates any
// further activity.
type Port interface {
	// AdmitRun records a new session's admission — the session row itself,
	// plus an optional session-scoped budget ceiling. Routing (which model,
	// and why) is a DATA-plane decision made before this call, using
	// internal/provider's router — this package cannot import that
	// (forbidden), so the resolved model id/reason arrive as plain strings,
	// already decided.
	AdmitRun(ctx context.Context, req AdmitRunV1) (AdmitRunResultV1, error)
	// ReserveBudget is internal/cost.Gate.Reserve, wrapped: reserve before a
	// model call, fail closed on an unpriced meter or an exhausted ceiling.
	ReserveBudget(ctx context.Context, req ReserveBudgetV1) (ReserveBudgetResultV1, error)
	// ReportCost is internal/cost.Gate.ReconcileUsage, wrapped: the real
	// usage from a completed call, reconciled against what ReserveBudget
	// reserved. ReservationID must name a reservation THIS Port instance
	// itself returned from ReserveBudget — LocalPort's own doc comment
	// explains why that can't be a stateless lookup.
	ReportCost(ctx context.Context, req ReportCostV1) error
	// EmitAuditReceipt is internal/audit.Chain.Append, wrapped: one
	// hash-chained receipt for one durably appended event.
	EmitAuditReceipt(ctx context.Context, req EmitAuditReceiptV1) error
	// RequestApproval is internal/oversight.Approvals.Create, wrapped: the
	// durable, decision-ready approval record a suspended tool_use is bound
	// to.
	RequestApproval(ctx context.Context, req RequestApprovalV1) (RequestApprovalResultV1, error)
	// AuthorizeContentAccess is internal/obs.Grants.RequestGrant, wrapped:
	// an audited, expiring grant onto a run's own sealed content — the
	// only path to plaintext outside the run's own audience.
	AuthorizeContentAccess(ctx context.Context, req AuthorizeContentAccessV1) (GrantV1, error)
}

// AdmitRunV1 is everything AdmitRun needs. BudgetUSD is a decimal string
// (e.g. "0.05", internal/cost.ParseDecimal's own input shape) rather than a
// internal/cost.Money value — Money is plain data too, but keeping the wire
// type a bare string here means a future non-Go control-plane client needs
// no internal/cost import at all to construct this request.
type AdmitRunV1 struct {
	TenantID      uuid.UUID
	UserID        uuid.UUID
	SessionID     uuid.UUID
	SurfaceID     string
	DataLabel     string
	RouteModelID  string
	RouteReason   map[string]string
	Autonomy      string
	BudgetUSD     string // empty = no session-scoped ceiling
	HarnessDigest []byte
	// Conversational is store.Session.Conversational's own value, set once
	// at admission and never changed — kernel.RunConfig.Conversational's
	// doc comment explains what it does. False for every caller except the
	// web chat UI.
	Conversational bool
}

// AdmitRunResultV1 reports whether the session was admitted — Admitted
// false with a Reason (an invalid autonomy value, an unparseable
// BudgetUSD) is a normal, non-error refusal; a returned error means the
// durable write itself failed.
type AdmitRunResultV1 struct {
	Admitted bool
	Reason   string
}

// ReserveBudgetV1 mirrors internal/cost.ReserveRequest field-for-field —
// Purpose travels as a bare string (internal/cost.Purpose's own underlying
// type), so this package doesn't need to re-export that type.
type ReserveBudgetV1 struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	ModelID   string
	Purpose   string
}

// ReserveBudgetResultV1 is internal/cost.Reservation's wire-safe
// projection: Decision/Reason/ReservedUSD (never a raw Money or Reservation
// value, which carries unexported fields a real RPC boundary couldn't
// serialize anyway). ReservationID is the correlator a later ReportCost
// call names.
type ReserveBudgetResultV1 struct {
	ReservationID uuid.UUID
	Decision      string // internal/cost.DecisionKind as a string: "allow" | "refuse_ceiling" | "degrade" | "skip"
	Reason        string
	ReservedUSD   string
}

// ReportCostV1 carries the four token-class counts internal/cost.Reconcile
// needs, as primitives — this package cannot import internal/provider
// (Usage's own package) to build that value directly; internal/cost.Gate.
// ReconcileUsage (README task 13.15) is the bridge that builds it
// server-side instead.
type ReportCostV1 struct {
	ReservationID   uuid.UUID
	InputUncached   int
	InputCacheRead  int
	InputCacheWrite int
	OutputTokens    int
	// Reported false is task 4.7's UNREPORTED case: a stream that failed
	// after the commit point, with no trustworthy usage figures — charged
	// at the full reserved worst case, never assumed free.
	Reported bool
}

// EmitAuditReceiptV1 mirrors internal/audit.Chain.Append's own parameters.
type EmitAuditReceiptV1 struct {
	TenantID      uuid.UUID
	SessionID     uuid.UUID
	Seq           int64
	EventID       uuid.UUID
	EventType     string
	PayloadDigest []byte
}

// RequestApprovalV1 mirrors internal/oversight.CreateApprovalRequest —
// Input is the tool_use's plaintext original input (never the sealed
// event payload; oversight's own doc comment on ContextPackage explains
// why an approver needs to see this, not a bare digest).
type RequestApprovalV1 struct {
	TenantID        uuid.UUID
	SessionID       uuid.UUID
	ToolUseEventID  uuid.UUID
	ToolID          string
	AskKind         string
	CanonicalDigest []byte
	EffectClass     string
	Input           json.RawMessage
}

type RequestApprovalResultV1 struct {
	ApprovalID uuid.UUID
	ExpiresAt  time.Time
}

// AuthorizeContentAccessV1 mirrors internal/obs.Grants.RequestGrant's own
// parameters.
type AuthorizeContentAccessV1 struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	GranteeID uuid.UUID
	Reason    string
	TTL       time.Duration
}

// GrantV1 is internal/obs.Grant's wire-safe projection.
type GrantV1 struct {
	GrantID   uuid.UUID
	ExpiresAt time.Time
}
