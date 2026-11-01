// Package postgres is the Postgres adapter for internal/delegate: the
// delegations and fanout_envelopes tables, with the session, key and event
// writes a spawn or a resolution makes in the same transaction. The rules —
// the depth and concurrency bounds, when a return is valid — are delegate's own.
//
// Bind and CreateEnvelope are transaction-level functions rather than Tx
// methods because their callers already hold a transaction of their own: the
// event log's OnDelegate hook (cmd/nexusd) binds inside the append that
// requests the delegation, and a plan step creates its envelope inside the
// step's own transaction.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditpg "github.com/truongpx396/nexus-agent-demo/internal/audit/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	cryptopg "github.com/truongpx396/nexus-agent-demo/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/sealed"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Store implements delegate.Store over Postgres.
type Store struct {
	st     *store.Store
	sealer *sealed.Sealer
}

var _ delegate.Store = (*Store)(nil)

// New returns a Store. keys seals each event under the session's own key and
// mints a child's key; chain may be nil, in which case events are appended
// without a signed receipt (a minimal wiring for tests).
func New(st *store.Store, keys *cryptopg.KeyStore, chain *auditpg.Chain) *Store {
	return &Store{st: st, sealer: sealed.NewSealer(keys, chain)}
}

// InTenant implements delegate.Store.
func (s *Store) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx delegate.Tx) error) error {
	return s.sealer.InTenant(ctx, s.st, tenantID, func(ctx context.Context, u *sealed.Unit) error {
		return fn(ctx, &txn{Unit: u})
	})
}

// txn is the delegate.Tx over one pgx transaction.
type txn struct{ *sealed.Unit }

var _ delegate.Tx = (*txn)(nil)

const delegationSelectSQL = `
	SELECT delegation_id, tenant_id, parent_session_id, child_session_id,
	       parent_tool_use_event_id, fanout_id, agent_id, task, scope_grant,
	       return_schema, status, result, reason, created_at, resolved_at
	FROM delegations`

type row interface {
	Scan(dest ...any) error
}

func scanDelegation(r row) (delegate.Delegation, error) {
	var d delegate.Delegation
	var scopeGrant, returnSchema, result []byte
	var toolUseEventID, fanoutID *uuid.UUID
	var reason *string
	err := r.Scan(
		&d.DelegationID, &d.TenantID, &d.ParentSessionID, &d.ChildSessionID,
		&toolUseEventID, &fanoutID, &d.AgentID, &d.Task, &scopeGrant,
		&returnSchema, &d.Status, &result, &reason, &d.CreatedAt, &d.ResolvedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return delegate.Delegation{}, fmt.Errorf("%w: delegation", delegate.ErrNotFound)
		}
		return delegate.Delegation{}, fmt.Errorf("delegate: scan delegation: %w", err)
	}
	if toolUseEventID != nil {
		d.ParentToolUseEventID = *toolUseEventID
	}
	d.FanoutID = fanoutID
	if len(scopeGrant) > 0 {
		if err := json.Unmarshal(scopeGrant, &d.ScopeGrant); err != nil {
			return delegate.Delegation{}, fmt.Errorf("delegate: unmarshal scope_grant: %w", err)
		}
	}
	if len(returnSchema) > 0 {
		d.ReturnSchema = returnSchema
	}
	if len(result) > 0 {
		d.Result = result
	}
	if reason != nil {
		d.Reason = *reason
	}
	return d, nil
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

func (t *txn) InsertDelegation(ctx context.Context, d delegate.Delegation) (delegate.Delegation, error) {
	if d.DelegationID == uuid.Nil {
		d.DelegationID = uuid.New()
	}
	scopeGrant, err := json.Marshal(d.ScopeGrant)
	if err != nil {
		return delegate.Delegation{}, fmt.Errorf("delegate: marshal scope_grant: %w", err)
	}
	d.Status = delegate.StatusPending
	err = t.Tx().QueryRow(ctx, `
		INSERT INTO delegations (
			delegation_id, tenant_id, parent_session_id, child_session_id,
			fanout_id, agent_id, task, scope_grant, return_schema, status
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING created_at`,
		d.DelegationID, d.TenantID, d.ParentSessionID, d.ChildSessionID,
		d.FanoutID, d.AgentID, d.Task, scopeGrant, nullableJSON(d.ReturnSchema), d.Status,
	).Scan(&d.CreatedAt)
	if err != nil {
		return delegate.Delegation{}, fmt.Errorf("delegate: create delegation: %w", err)
	}
	return d, nil
}

func (t *txn) GetDelegation(ctx context.Context, delegationID uuid.UUID) (delegate.Delegation, error) {
	return scanDelegation(t.Tx().QueryRow(ctx, delegationSelectSQL+` WHERE delegation_id = $1`, delegationID))
}

func (t *txn) FindPendingByChild(ctx context.Context, childSessionID uuid.UUID) (delegate.Delegation, bool, error) {
	d, err := scanDelegation(t.Tx().QueryRow(ctx, delegationSelectSQL+` WHERE child_session_id = $1 AND status = 'pending' FOR UPDATE`, childSessionID))
	if err != nil {
		if errors.Is(err, delegate.ErrNotFound) {
			return delegate.Delegation{}, false, nil
		}
		return delegate.Delegation{}, false, err
	}
	return d, true, nil
}

func (t *txn) ListFanout(ctx context.Context, fanoutID uuid.UUID) ([]delegate.Delegation, error) {
	rows, err := t.Tx().Query(ctx, delegationSelectSQL+` WHERE fanout_id = $1 ORDER BY created_at ASC`, fanoutID)
	if err != nil {
		return nil, fmt.Errorf("delegate: list fanout %s: %w", fanoutID, err)
	}
	defer rows.Close()
	var out []delegate.Delegation
	for rows.Next() {
		dg, err := scanDelegation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dg)
	}
	return out, rows.Err()
}

func (t *txn) CountForRoot(ctx context.Context, rootSessionID uuid.UUID) (open, total int, err error) {
	err = t.Tx().QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE d.status = 'pending'),
			count(*)
		FROM delegations d
		JOIN sessions s ON s.session_id = d.parent_session_id
		WHERE s.root_session_id = $1`, rootSessionID,
	).Scan(&open, &total)
	return open, total, err
}

func (t *txn) SetDelegationResult(ctx context.Context, delegationID uuid.UUID, status delegate.Status, result json.RawMessage, reason string) error {
	_, err := t.Tx().Exec(ctx, `
		UPDATE delegations SET status = $2, result = $3, reason = $4, resolved_at = now()
		WHERE delegation_id = $1`,
		delegationID, status, nullableJSON(result), nullIfEmpty(reason),
	)
	if err != nil {
		return fmt.Errorf("delegate: resolve delegation %s: %w", delegationID, err)
	}
	return nil
}

func (t *txn) Resolvable(ctx context.Context, childSessionID uuid.UUID) (bool, error) {
	var bound bool
	err := t.Tx().QueryRow(ctx, `
		SELECT fanout_id IS NOT NULL OR parent_tool_use_event_id IS NOT NULL
		FROM delegations WHERE child_session_id = $1`,
		childSessionID,
	).Scan(&bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // no delegation row at all: nothing to wait for
	}
	if err != nil {
		return false, err
	}
	return bound, nil
}

// Bind sets parent_tool_use_event_id — called from kernel.OnDelegate, right
// after suspendForDelegation appends EventDelegationRequested (README task
// 8.10), so a delegations row is never left unbound to the tool_use it gates
// for longer than one transaction.
func Bind(ctx context.Context, tx pgx.Tx, childSessionID, toolUseEventID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE delegations SET parent_tool_use_event_id = $2
		WHERE child_session_id = $1 AND status = 'pending'`,
		childSessionID, toolUseEventID,
	)
	if err != nil {
		return fmt.Errorf("delegate: bind delegation for child %s: %w", childSessionID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delegate: bind delegation for child %s: no pending row found", childSessionID)
	}
	return nil
}

// CreateEnvelope reserves a delegate_fanout step's whole cohort ceiling BEFORE
// the first child starts (README task 8.13) — a plain Postgres row, never
// internal/cost's tenant-scoped Redis counter: every child's own turn-level draw
// decrements THIS row instead, which is what makes "per-child reservation
// against the tenant counter is prohibited" true by construction — a fan-out
// never makes more than this one call against the shared tenant ceiling.
func CreateEnvelope(ctx context.Context, tx pgx.Tx, tenantID, planSessionID uuid.UUID, ceiling cost.Money, childCount int) (uuid.UUID, error) {
	envelopeID := uuid.New()
	_, err := tx.Exec(ctx, `
		INSERT INTO fanout_envelopes (envelope_id, tenant_id, plan_session_id, currency, ceiling_micros, remaining_micros, child_count)
		VALUES ($1,$2,$3,$4,$5,$5,$6)`,
		envelopeID, tenantID, planSessionID, ceiling.Currency, ceiling.Micros, childCount,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("delegate: create fanout envelope: %w", err)
	}
	return envelopeID, nil
}

// DrawEnvelope atomically decrements remaining_micros by amount, refusing
// (ok=false) rather than going negative — the CHECK (remaining_micros >= 0)
// constraint backs this up at the schema level too, but the conditional UPDATE
// is what turns "refuse" into a normal, expected outcome instead of a
// constraint-violation error.
func (t *txn) DrawEnvelope(ctx context.Context, envelopeID uuid.UUID, amount cost.Money) (bool, cost.Money, error) {
	var remainingMicros int64
	var currency string
	err := t.Tx().QueryRow(ctx, `
		UPDATE fanout_envelopes SET remaining_micros = remaining_micros - $2
		WHERE envelope_id = $1 AND remaining_micros >= $2
		RETURNING remaining_micros, currency`,
		envelopeID, amount.Micros,
	).Scan(&remainingMicros, &currency)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either the envelope doesn't exist, or it exists but doesn't have
			// amount left — read the current balance to report it and to
			// distinguish "exhausted" from "no such envelope" in the caller's
			// error, without a second round trip on the common path.
			var cur int64
			var cy string
			if qerr := t.Tx().QueryRow(ctx, `SELECT remaining_micros, currency FROM fanout_envelopes WHERE envelope_id = $1`, envelopeID).Scan(&cur, &cy); qerr != nil {
				return false, cost.Money{}, fmt.Errorf("delegate: envelope %s not found", envelopeID)
			}
			return false, cost.Money{Micros: cur, Currency: cy}, nil
		}
		return false, cost.Money{}, fmt.Errorf("delegate: draw from envelope %s: %w", envelopeID, err)
	}
	return true, cost.Money{Micros: remainingMicros, Currency: currency}, nil
}

func (t *txn) ReturnToEnvelope(ctx context.Context, envelopeID uuid.UUID, amount cost.Money) error {
	_, err := t.Tx().Exec(ctx, `UPDATE fanout_envelopes SET remaining_micros = remaining_micros + $2 WHERE envelope_id = $1`, envelopeID, amount.Micros)
	return err
}

func (t *txn) EnvelopeShare(ctx context.Context, envelopeID uuid.UUID) (cost.Money, int, error) {
	var ceilingMicros int64
	var currency string
	var childCount int
	err := t.Tx().QueryRow(ctx, `SELECT ceiling_micros, currency, child_count FROM fanout_envelopes WHERE envelope_id = $1`, envelopeID).
		Scan(&ceilingMicros, &currency, &childCount)
	if err != nil {
		return cost.Money{}, 0, fmt.Errorf("delegate: load envelope %s: %w", envelopeID, err)
	}
	return cost.Money{Micros: ceilingMicros, Currency: currency}, childCount, nil
}
