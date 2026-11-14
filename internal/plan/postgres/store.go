// Package postgres is the Postgres adapter for internal/plan: the
// orchestration_plans and plan_sessions tables, with the session, key and event
// writes a plan run makes in the same transaction. The rules — the lifecycle's order, what a
// transition fires — are plan's own.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditpg "github.com/truongpx396/nexus-agent/internal/audit/postgres"
	"github.com/truongpx396/nexus-agent/internal/cost"
	cryptopg "github.com/truongpx396/nexus-agent/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent/internal/eventlog"
	"github.com/truongpx396/nexus-agent/internal/eventlog/sealed"
	"github.com/truongpx396/nexus-agent/internal/plan"
	"github.com/truongpx396/nexus-agent/internal/store"
	"github.com/truongpx396/nexus-agent/kernel"
)

// Store implements plan.Store over Postgres.
type Store struct {
	st        *store.Store
	sealer    *sealed.Sealer
	envelopes EnvelopeWriter
}

// EnvelopeWriter creates a fan-out envelope row inside the caller's
// transaction. The fanout_envelopes table belongs to internal/delegate, so the
// implementation (delegate/postgres.CreateEnvelope) is injected rather than
// imported: this adapter does not reach into another feature's adapter.
type EnvelopeWriter func(ctx context.Context, tx pgx.Tx, tenantID, planSessionID uuid.UUID, ceiling cost.Money, childCount int) (uuid.UUID, error)

var _ plan.Store = (*Store)(nil)

// New returns a Store. keys seals each event under the session's own key; chain
// may be nil, in which case events are appended without a signed receipt (a
// minimal wiring for tests). envelopes creates the fan-out envelope a plan's
// parallel step reserves its budget in; nil means the plan has no fan-out step,
// and CreateFanoutEnvelope then refuses.
func New(st *store.Store, keys *cryptopg.KeyStore, chain *auditpg.Chain, envelopes EnvelopeWriter) *Store {
	return &Store{st: st, sealer: sealed.NewSealer(keys, chain), envelopes: envelopes}
}

// InTenant implements plan.Store.
func (s *Store) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx plan.Tx) error) error {
	return s.sealer.InTenant(ctx, s.st, tenantID, func(ctx context.Context, u *sealed.Unit) error {
		return fn(ctx, &txn{Unit: u, envelopes: s.envelopes})
	})
}

// txn is the plan.Tx over one pgx transaction.
type txn struct {
	*sealed.Unit
	envelopes EnvelopeWriter
}

var _ plan.Tx = (*txn)(nil)

func (t *txn) AppendFirst(ctx context.Context, tenantID, sessionID uuid.UUID, seal kernel.SealFunc, typ eventlog.EventType, payload any) (eventlog.Event, error) {
	return t.Unit.AppendFirst(ctx, tenantID, sessionID, seal, typ, payload)
}

func (t *txn) CreateFanoutEnvelope(ctx context.Context, tenantID, planSessionID uuid.UUID, ceiling cost.Money, childCount int) (uuid.UUID, error) {
	if t.envelopes == nil {
		return uuid.Nil, errors.New("plan: a fan-out envelope was requested but no envelope writer is wired")
	}
	return t.envelopes(ctx, t.Tx(), tenantID, planSessionID, ceiling, childCount)
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (t *txn) InsertPlan(ctx context.Context, tenantID, planID uuid.UUID, version int, name string, spec []byte, status plan.Status, createdBy string) error {
	_, err := t.Tx().Exec(ctx, `
		INSERT INTO orchestration_plans (plan_id, tenant_id, version, name, spec, status, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		planID, tenantID, version, name, spec, status, nullIfEmpty(createdBy),
	)
	return err
}

func (t *txn) PlanSpec(ctx context.Context, planID uuid.UUID, version int) ([]byte, plan.Status, bool, error) {
	var row pgx.Row
	if version > 0 {
		row = t.Tx().QueryRow(ctx, `SELECT spec, status FROM orchestration_plans WHERE plan_id = $1 AND version = $2`, planID, version)
	} else {
		row = t.Tx().QueryRow(ctx, `SELECT spec, status FROM orchestration_plans WHERE plan_id = $1 ORDER BY version DESC LIMIT 1`, planID)
	}
	return scanSpec(row)
}

func (t *txn) LockPlan(ctx context.Context, planID uuid.UUID, version int) ([]byte, plan.Status, bool, error) {
	return scanSpec(t.Tx().QueryRow(ctx, `SELECT spec, status FROM orchestration_plans WHERE plan_id = $1 AND version = $2 FOR UPDATE`, planID, version))
}

func scanSpec(row pgx.Row) ([]byte, plan.Status, bool, error) {
	var spec []byte
	var status plan.Status
	if err := row.Scan(&spec, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	return spec, status, true, nil
}

func (t *txn) SetPlan(ctx context.Context, planID uuid.UUID, version int, spec []byte, status plan.Status) error {
	_, err := t.Tx().Exec(ctx, `UPDATE orchestration_plans SET spec = $3, status = $4 WHERE plan_id = $1 AND version = $2`, planID, version, spec, status)
	return err
}

func (t *txn) SetSignedOffBy(ctx context.Context, planID uuid.UUID, version int, by string) error {
	_, err := t.Tx().Exec(ctx, `UPDATE orchestration_plans SET signed_off_by = $3 WHERE plan_id = $1 AND version = $2`, planID, version, by)
	return err
}

func (t *txn) SetEnabledPins(ctx context.Context, planID uuid.UUID, version, agentVersion int, routeModelID string) error {
	_, err := t.Tx().Exec(ctx, `UPDATE orchestration_plans SET agent_version = $3, route_model_id = $4, enabled_at = now() WHERE plan_id = $1 AND version = $2`,
		planID, version, agentVersion, routeModelID)
	return err
}

func (t *txn) RetirePlan(ctx context.Context, planID uuid.UUID, version int) (bool, error) {
	tag, err := t.Tx().Exec(ctx, `UPDATE orchestration_plans SET status = $3 WHERE plan_id = $1 AND version = $2 AND status = $4`,
		planID, version, plan.StatusRetired, plan.StatusEnabled)
	if err != nil {
		return false, fmt.Errorf("plan: retire %s version %d: %w", planID, version, err)
	}
	return tag.RowsAffected() > 0, nil
}
