package plan

import (
	"context"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent/internal/cost"
	"github.com/truongpx396/nexus-agent/internal/crypto"
	"github.com/truongpx396/nexus-agent/internal/delegate"
	"github.com/truongpx396/nexus-agent/internal/eventlog"
	"github.com/truongpx396/nexus-agent/internal/oversight"
	"github.com/truongpx396/nexus-agent/kernel"
)

// Tx is one open unit of work in a single tenant's scope: the orchestration
// plan rows the Lifecycle moves through its states, and what the Executor reads
// and changes while it walks a plan, so that a plan run's events, its driving
// session's status and a plan's governance record commit together or not at all.
// The Postgres implementation is internal/plan/postgres, a pgx transaction under
// the tenant's row-level security; the rules — the lifecycle's order, what a
// transition fires — are this package's.
type Tx interface {
	eventlog.Appender
	eventlog.SessionRows
	crypto.Keys

	// AppendFirst appends the session's very first event, sealed by seal: unlike
	// every later append it has no earlier event to find its key from
	// (Executor.Start's own doc comment).
	AppendFirst(ctx context.Context, tenantID, sessionID uuid.UUID, seal kernel.SealFunc, typ eventlog.EventType, payload any) (eventlog.Event, error)

	// PinSession records the plan version sessionID is a run of, so that a later
	// edit to the plan can never change what an in-flight run executes (task
	// 8.4). The pin is plan's own record: the session aggregate does not carry it.
	PinSession(ctx context.Context, tenantID, sessionID, planID uuid.UUID, version int) error

	// PinnedPlan is the plan version sessionID was pinned to; ok=false means it
	// is not a plan-driven session.
	PinnedPlan(ctx context.Context, sessionID uuid.UUID) (planID uuid.UUID, version int, ok bool, err error)

	// CreateFanoutEnvelope reserves a delegate_fanout step's whole cohort
	// ceiling before its first child starts (task 8.13).
	CreateFanoutEnvelope(ctx context.Context, tenantID, planSessionID uuid.UUID, ceiling cost.Money, childCount int) (uuid.UUID, error)

	// InsertPlan stores a new plan version with its spec already marshaled.
	InsertPlan(ctx context.Context, tenantID, planID uuid.UUID, version int, name string, spec []byte, status Status, createdBy string) error

	// PlanSpec loads a plan version's spec and status; version 0 means the
	// latest. found=false is "no such plan or version".
	PlanSpec(ctx context.Context, planID uuid.UUID, version int) (spec []byte, status Status, found bool, err error)

	// LockPlan is PlanSpec for an exact version, locked for update so that two
	// transitions of the same plan serialize.
	LockPlan(ctx context.Context, planID uuid.UUID, version int) (spec []byte, status Status, found bool, err error)

	// SetPlan replaces a plan version's spec and status.
	SetPlan(ctx context.Context, planID uuid.UUID, version int, spec []byte, status Status) error

	// SetSignedOffBy records who signed a plan version off.
	SetSignedOffBy(ctx context.Context, planID uuid.UUID, version int, by string) error

	// SetEnabledPins records the agent version and model a plan version was
	// enabled with, and when.
	SetEnabledPins(ctx context.Context, planID uuid.UUID, version, agentVersion int, routeModelID string) error

	// RetirePlan moves an enabled plan version to retired; retired=false means
	// it was not enabled.
	RetirePlan(ctx context.Context, planID uuid.UUID, version int) (retired bool, err error)
}

// Store runs a unit of work in a tenant's scope: it opens the unit, calls fn
// with a Tx bound to it, and commits if fn returns nil or rolls back if not.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx Tx) error) error
}

// The three collaborators the Executor drives while it walks a plan, each as
// the narrow slice of another feature it actually calls. The feature's own
// service (oversight.Approvals, oversight.Inputs, delegate.Delegations)
// satisfies them; naming them here is what lets a plan be exercised against a
// fake, and what keeps this package from depending on another service's whole
// surface. Nil means the plan has no step of that kind (the step is refused); assign only a non-nil service, because a nil pointer stored in one of these interfaces is a non-nil interface and passes that check.

// ApprovalOpener opens an approval a gate or agent step suspends on.
type ApprovalOpener interface {
	Create(ctx context.Context, req oversight.CreateApprovalRequest) (oversight.Approval, error)
}

// InputRequester asks the human a question an input step suspends on.
type InputRequester interface {
	RequestInput(ctx context.Context, p oversight.RequestInputParams) (oversight.InputRequest, error)
}

// Delegator spawns the child sessions of a fan-out step and reports which of a
// fan-out's children are still open.
type Delegator interface {
	Spawn(ctx context.Context, req delegate.SpawnRequest) (uuid.UUID, error)
	ListOpenForFanout(ctx context.Context, tenantID, fanoutID uuid.UUID) ([]delegate.Delegation, error)
}
