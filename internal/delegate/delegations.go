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

// ParentContext reads sessionID's depth and root_session_id — everything
// platform/delegate's own CheckPermissions needs to enforce the depth bound
// (README task 8.12) before it ever asks CountForRoot about the other two.
func (d *Delegations) ParentContext(ctx context.Context, tenantID, sessionID uuid.UUID) (depth int, rootSessionID uuid.UUID, err error) {
	err = d.Store.InTenant(ctx, tenantID, func(ctx context.Context, tx Tx) error {
		sess, gerr := tx.GetSession(ctx, sessionID)
		if gerr != nil {
			return gerr
		}
		depth, rootSessionID = sess.Depth, sess.RootSessionID
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
