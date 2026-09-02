package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Delegations is the delegation transaction (README tasks 8.8-8.14):
// tracking a spawned child from creation through resolution. Construct once,
// share across the process — the same convention every other transactional
// component in this codebase (oversight.Approvals, cost.Gate, ...) follows.
type Delegations struct {
	deps
	cfg Config
}

func NewDelegations(st *store.Store, keys *crypto.KeyStore, chain *audit.Chain) *Delegations {
	return &Delegations{deps: deps{Store: st, Keys: keys, Chain: chain}}
}

// ParentContext reads sessionID's depth and root_session_id — everything
// platform/delegate's own CheckPermissions needs to enforce the depth bound
// (README task 8.12) before it ever asks CountForRoot about the other two.
func (d *Delegations) ParentContext(ctx context.Context, tenantID, sessionID uuid.UUID) (depth int, rootSessionID uuid.UUID, err error) {
	err = d.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		sess, gerr := store.GetSession(ctx, tx, sessionID)
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
	err = d.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT
				count(*) FILTER (WHERE d.status = 'pending'),
				count(*)
			FROM delegations d
			JOIN sessions s ON s.session_id = d.parent_session_id
			WHERE s.root_session_id = $1`, rootSessionID,
		).Scan(&open, &total)
	})
	return open, total, err
}

// Get loads one delegation by id.
func (d *Delegations) Get(ctx context.Context, tenantID, delegationID uuid.UUID) (Delegation, error) {
	var out Delegation
	err := d.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = scanDelegation(tx.QueryRow(ctx, delegationSelectSQL+` WHERE delegation_id = $1`, delegationID))
		return err
	})
	return out, err
}

// findPendingByChild locates the (at most one) pending delegation naming
// childSessionID — Resolve's own lookup key, since a child session is
// exactly one delegation's child, ever.
func findPendingByChild(ctx context.Context, tx pgx.Tx, childSessionID uuid.UUID) (Delegation, bool, error) {
	d, err := scanDelegation(tx.QueryRow(ctx, delegationSelectSQL+` WHERE child_session_id = $1 AND status = 'pending' FOR UPDATE`, childSessionID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Delegation{}, false, nil
		}
		return Delegation{}, false, err
	}
	return d, true, nil
}

// ListOpenForFanout returns every delegation sharing fanoutID, in creation
// order — internal/plan's own delegate_fanout step handler uses this to
// check whether an entire cohort has resolved.
func (d *Delegations) ListOpenForFanout(ctx context.Context, tenantID, fanoutID uuid.UUID) ([]Delegation, error) {
	var out []Delegation
	err := d.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, delegationSelectSQL+` WHERE fanout_id = $1 ORDER BY created_at ASC`, fanoutID)
		if err != nil {
			return fmt.Errorf("delegate: list fanout %s: %w", fanoutID, err)
		}
		defer rows.Close()
		for rows.Next() {
			dg, err := scanDelegation(rows)
			if err != nil {
				return err
			}
			out = append(out, dg)
		}
		return rows.Err()
	})
	return out, err
}

// create inserts a new pending delegation row — parent_tool_use_event_id
// starts NULL; Bind fills it in once the gating tool_use event exists
// (kernel/loop.go's suspendForDelegation runs AFTER Spawn, so the tool_use's
// own EventID isn't known yet at create time).
func create(ctx context.Context, tx pgx.Tx, d Delegation) (Delegation, error) {
	if d.DelegationID == uuid.Nil {
		d.DelegationID = uuid.New()
	}
	scopeGrant, err := json.Marshal(d.ScopeGrant)
	if err != nil {
		return Delegation{}, fmt.Errorf("delegate: marshal scope_grant: %w", err)
	}
	d.Status = StatusPending
	err = tx.QueryRow(ctx, `
		INSERT INTO delegations (
			delegation_id, tenant_id, parent_session_id, child_session_id,
			fanout_id, agent_id, task, scope_grant, return_schema, status
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING created_at`,
		d.DelegationID, d.TenantID, d.ParentSessionID, d.ChildSessionID,
		d.FanoutID, d.AgentID, d.Task, scopeGrant, nullableJSON(d.ReturnSchema), d.Status,
	).Scan(&d.CreatedAt)
	if err != nil {
		return Delegation{}, fmt.Errorf("delegate: create delegation: %w", err)
	}
	return d, nil
}

// Bind sets parent_tool_use_event_id — called from kernel.OnDelegate, right
// after suspendForDelegation appends EventDelegationRequested (README task
// 8.10), so a delegations row is never left unbound to the tool_use it
// gates for longer than one transaction.
func (d *Delegations) Bind(ctx context.Context, tx pgx.Tx, delegationChildSessionID, toolUseEventID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE delegations SET parent_tool_use_event_id = $2
		WHERE child_session_id = $1 AND status = 'pending'`,
		delegationChildSessionID, toolUseEventID,
	)
	if err != nil {
		return fmt.Errorf("delegate: bind delegation for child %s: %w", delegationChildSessionID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delegate: bind delegation for child %s: no pending row found", delegationChildSessionID)
	}
	return nil
}

func resolveStatus(ctx context.Context, tx pgx.Tx, delegationID uuid.UUID, status Status, result json.RawMessage, reason string) error {
	_, err := tx.Exec(ctx, `
		UPDATE delegations SET status = $2, result = $3, reason = $4, resolved_at = now()
		WHERE delegation_id = $1`,
		delegationID, status, nullableJSON(result), nullIfEmpty(reason),
	)
	if err != nil {
		return fmt.Errorf("delegate: resolve delegation %s: %w", delegationID, err)
	}
	return nil
}

func nullableJSON(b json.RawMessage) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
