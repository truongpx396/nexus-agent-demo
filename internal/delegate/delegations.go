package delegate

import (
	"context"

	"github.com/google/uuid"
)

// Delegations is the delegation transaction (README tasks 8.8-8.14):
// tracking a spawned child from creation through resolution. Construct once,
// share across the process — the same convention every other transactional
// component in this codebase (oversight.Approvals, cost.Gate, ...) follows.
type Delegations struct {
	Store Store
	cfg   Config
}

// NewDelegations returns Delegations over store.
func NewDelegations(store Store) *Delegations { return &Delegations{Store: store} }

// ParentContext reads sessionID's depth and root session — everything
// platform/delegate's own CheckPermissions needs to enforce the depth bound
// (README task 8.12) before it ever asks CountForRoot about the other two. The
// session is loaded first so that one that does not exist in the tenant's scope
// is an error, not a root at depth 0.
func (d *Delegations) ParentContext(ctx context.Context, tenantID, sessionID uuid.UUID) (depth int, rootSessionID uuid.UUID, err error) {
	err = d.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		if _, gerr := tx.GetSession(ctx, sessionID); gerr != nil {
			return gerr
		}
		l, lerr := tx.LineageOf(ctx, sessionID)
		if lerr != nil {
			return lerr
		}
		depth, rootSessionID = l.Depth, l.Root
		return nil
	})
	return depth, rootSessionID, err
}

// CountForRoot returns how many delegations rows exist for rootSessionID —
// open (still status=pending, the concurrent bound) and total (any status
// ever, the per_run bound). README task 8.12: both fail closed at MaxConcurrent/MaxPerRun.
func (d *Delegations) CountForRoot(ctx context.Context, tenantID, rootSessionID uuid.UUID) (open, total int, err error) {
	err = d.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var cerr error
		open, total, cerr = tx.CountForRoot(ctx, rootSessionID)
		return cerr
	})
	return open, total, err
}

// BindToolUse binds the pending delegation of childSessionID to the tool_use
// event that gates it. The run calls it right after appending
// delegation_requested (README task 8.10), so a delegation is never left
// unbound to the tool_use it gates for longer than one transaction, and the
// child's resolution always finds the parent call to resume. The event log's
// suspension hook is how the kernel reaches it; this is the use case behind it.
func (d *Delegations) BindToolUse(ctx context.Context, tenantID, childSessionID, toolUseEventID uuid.UUID) error {
	return d.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		return tx.BindToolUse(ctx, childSessionID, toolUseEventID)
	})
}

// Get loads one delegation by id.
func (d *Delegations) Get(ctx context.Context, tenantID, delegationID uuid.UUID) (Delegation, error) {
	var out Delegation
	err := d.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		out, err = tx.GetDelegation(ctx, delegationID)
		return err
	})
	return out, err
}

// ListOpenForFanout returns every delegation sharing fanoutID, in creation
// order — internal/plan's own delegate_fanout step handler uses this to
// check whether an entire cohort has resolved.
func (d *Delegations) ListOpenForFanout(ctx context.Context, tenantID, fanoutID uuid.UUID) ([]Delegation, error) {
	var out []Delegation
	err := d.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		var err error
		out, err = tx.ListFanout(ctx, fanoutID)
		return err
	})
	return out, err
}
