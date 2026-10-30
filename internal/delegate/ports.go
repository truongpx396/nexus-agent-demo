package delegate

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// ErrNotFound mirrors internal/oversight.ErrNotFound's own convention: "it
// never existed" and "RLS made a cross-tenant row invisible" are
// indistinguishable on purpose.
var ErrNotFound = errors.New("delegate: not found")

// Tx is one open unit of work in a single tenant's scope: everything spawning,
// tracking and resolving a delegation reads and changes, so that a delegation's
// status, the events appended to its parent, and the child's session row commit
// together or not at all. The Postgres implementation is
// internal/delegate/postgres, a pgx transaction under the tenant's row-level
// security; the rules — the bounds, when a return is valid, what a returned
// delegation appends — are this package's.
type Tx interface {
	eventlog.Appender
	eventlog.SessionRows
	crypto.Keys

	// InsertDelegation records a new pending delegation (the row starts with no
	// parent_tool_use_event_id; Bind fills it in once the gating tool_use
	// exists) and returns it with its id and creation time set.
	InsertDelegation(ctx context.Context, d Delegation) (Delegation, error)

	// GetDelegation loads one delegation, or an error wrapping ErrNotFound.
	GetDelegation(ctx context.Context, delegationID uuid.UUID) (Delegation, error)

	// FindPendingByChild locks and returns the (at most one) pending delegation
	// naming childSessionID; found=false means there is none.
	FindPendingByChild(ctx context.Context, childSessionID uuid.UUID) (d Delegation, found bool, err error)

	// ListFanout returns every delegation sharing fanoutID, in creation order.
	ListFanout(ctx context.Context, fanoutID uuid.UUID) ([]Delegation, error)

	// CountForRoot is how many delegations hang off rootSessionID: those still
	// pending (the concurrent bound) and all of them (the per-run bound).
	CountForRoot(ctx context.Context, rootSessionID uuid.UUID) (open, total int, err error)

	// SetDelegationResult resolves a delegation to status with its result and
	// reason.
	SetDelegationResult(ctx context.Context, delegationID uuid.UUID, status Status, result json.RawMessage, reason string) error

	// Resolvable reports whether the delegation naming childSessionID, if it has
	// one, can be resolved: it is bound to its gating tool_use, or it is a
	// fanout child (whose parent is a plan step rather than a tool_use). A
	// session that is no delegation's child at all has nothing to wait for.
	Resolvable(ctx context.Context, childSessionID uuid.UUID) (bool, error)

	// DrawEnvelope atomically takes amount from a fanout envelope, refusing
	// (ok=false, with what is left) rather than going negative. An envelope that
	// does not exist is an error.
	DrawEnvelope(ctx context.Context, envelopeID uuid.UUID, amount cost.Money) (ok bool, remaining cost.Money, err error)

	// ReturnToEnvelope gives amount back to a fanout envelope.
	ReturnToEnvelope(ctx context.Context, envelopeID uuid.UUID, amount cost.Money) error

	// EnvelopeShare is an envelope's ceiling and how many children share it.
	EnvelopeShare(ctx context.Context, envelopeID uuid.UUID) (ceiling cost.Money, childCount int, err error)
}

// Store runs a unit of work in a tenant's scope: it opens the unit, calls fn
// with a Tx bound to it, and commits if fn returns nil or rolls back if not.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx Tx) error) error
}

// PriceBooks is the part of cost.Ledger an envelope needs to price a child's
// real usage.
type PriceBooks interface {
	LoadPriceBook(ctx context.Context, tenantID uuid.UUID) (*cost.PriceBook, error)
}
