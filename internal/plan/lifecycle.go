package plan

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Status is orchestration_plans.status's vocabulary (README task 8.4):
// draft -> validated -> eval_passed -> signed_off -> enabled -> retired.
// Once a version reaches Enabled, Lifecycle refuses every further
// transition on it except Retire — "enabled versions immutable."
type Status string

const (
	StatusDraft      Status = "draft"
	StatusValidated  Status = "validated"
	StatusEvalPassed Status = "eval_passed"
	StatusSignedOff  Status = "signed_off"
	StatusEnabled    Status = "enabled"
	StatusRetired    Status = "retired"
)

// EvalGate is the eval-gate step in the lifecycle (README task 8.4) —
// deliberately a plain function rather than an import of package evals, so
// this package doesn't need to depend on evals' own corpus/runner shape;
// cmd/nexusd wires the real evals.Report-backed gate in. passed=false with
// a nil error is an ordinary gate failure (stays at StatusValidated, fixable
// and re-runnable); a non-nil error means the gate itself couldn't run.
type EvalGate func(ctx context.Context, p Plan) (passed bool, detail string, err error)

// Lifecycle owns the orchestration_plans transaction.
type Lifecycle struct {
	Store *store.Store
}

// Create inserts a new plan at version 1, status=draft.
func (l *Lifecycle) Create(ctx context.Context, tenantID uuid.UUID, p Plan, createdBy string) (Plan, error) {
	if p.PlanID == uuid.Nil {
		p.PlanID = uuid.New()
	}
	p.TenantID = tenantID
	p.Version = 1
	return p, l.insert(ctx, tenantID, p, StatusDraft, createdBy)
}

// NewVersion inserts the next version of an existing plan_id, also starting
// at status=draft — a new version never inherits the prior one's lifecycle
// progress (task 8.4's "in-flight runs finish on their version" only makes
// sense if a later version's own gate/sign-off is independently earned).
func (l *Lifecycle) NewVersion(ctx context.Context, tenantID, planID uuid.UUID, p Plan, createdBy string) (Plan, error) {
	latest, _, err := l.Get(ctx, tenantID, planID, 0)
	if err != nil {
		return Plan{}, err
	}
	p.PlanID = planID
	p.TenantID = tenantID
	p.Version = latest.Version + 1
	return p, l.insert(ctx, tenantID, p, StatusDraft, createdBy)
}

func (l *Lifecycle) insert(ctx context.Context, tenantID uuid.UUID, p Plan, status Status, createdBy string) error {
	spec, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("plan: marshal spec: %w", err)
	}
	return l.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO orchestration_plans (plan_id, tenant_id, version, name, spec, status, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			p.PlanID, tenantID, p.Version, p.Name, spec, status, nullIfEmpty(createdBy),
		)
		return err
	})
}

// Get loads a plan version — version=0 means "the latest version for this
// plan_id."
func (l *Lifecycle) Get(ctx context.Context, tenantID, planID uuid.UUID, version int) (Plan, Status, error) {
	var specJSON []byte
	var status Status
	err := l.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var row pgx.Row
		if version > 0 {
			row = tx.QueryRow(ctx, `SELECT spec, status FROM orchestration_plans WHERE plan_id = $1 AND version = $2`, planID, version)
		} else {
			row = tx.QueryRow(ctx, `SELECT spec, status FROM orchestration_plans WHERE plan_id = $1 ORDER BY version DESC LIMIT 1`, planID)
		}
		return row.Scan(&specJSON, &status)
	})
	if err != nil {
		if err == pgx.ErrNoRows {
			return Plan{}, "", fmt.Errorf("plan: %s version %d not found", planID, version)
		}
		return Plan{}, "", fmt.Errorf("plan: get %s version %d: %w", planID, version, err)
	}
	var p Plan
	if err := json.Unmarshal(specJSON, &p); err != nil {
		return Plan{}, "", fmt.Errorf("plan: unmarshal spec: %w", err)
	}
	return p, status, nil
}

// transition loads the current status, checks it against from, runs fn
// (which may mutate p before it's re-marshaled), and writes the new status
// — all inside one tenant-scoped transaction, refusing outright once the
// row is Enabled or Retired (immutability, task 8.4).
func (l *Lifecycle) transition(ctx context.Context, tenantID, planID uuid.UUID, version int, from, to Status, mutate func(*Plan)) error {
	return l.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var specJSON []byte
		var status Status
		if err := tx.QueryRow(ctx, `SELECT spec, status FROM orchestration_plans WHERE plan_id = $1 AND version = $2 FOR UPDATE`, planID, version).Scan(&specJSON, &status); err != nil {
			return fmt.Errorf("plan: load %s version %d: %w", planID, version, err)
		}
		if status == StatusEnabled || status == StatusRetired {
			return fmt.Errorf("plan: %s version %d is %s; enabled/retired versions are immutable", planID, version, status)
		}
		if status != from {
			return fmt.Errorf("plan: %s version %d is %s, not %s", planID, version, status, from)
		}
		var p Plan
		if err := json.Unmarshal(specJSON, &p); err != nil {
			return fmt.Errorf("plan: unmarshal spec: %w", err)
		}
		if mutate != nil {
			mutate(&p)
		}
		newSpec, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("plan: marshal spec: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE orchestration_plans SET spec = $3, status = $4 WHERE plan_id = $1 AND version = $2`, planID, version, newSpec, to)
		return err
	})
}

// ValidatePlan runs Validate (validate.go) against the stored spec and, on
// success, advances draft -> validated.
func (l *Lifecycle) ValidatePlan(ctx context.Context, tenantID, planID uuid.UUID, version int) error {
	p, status, err := l.Get(ctx, tenantID, planID, version)
	if err != nil {
		return err
	}
	if status != StatusDraft {
		return fmt.Errorf("plan: %s version %d is %s, not draft", planID, version, status)
	}
	if err := Validate(p); err != nil {
		return fmt.Errorf("plan: validation failed: %w", err)
	}
	return l.transition(ctx, tenantID, planID, version, StatusDraft, StatusValidated, nil)
}

// RunEvalGate runs gate against the stored spec and, on a pass, advances
// validated -> eval_passed. A gate FAILURE (passed=false, err=nil) is
// reported to the caller but leaves the plan at StatusValidated — fixable
// and re-runnable, never a dead end.
func (l *Lifecycle) RunEvalGate(ctx context.Context, tenantID, planID uuid.UUID, version int, gate EvalGate) (passed bool, detail string, err error) {
	p, status, err := l.Get(ctx, tenantID, planID, version)
	if err != nil {
		return false, "", err
	}
	if status != StatusValidated {
		return false, "", fmt.Errorf("plan: %s version %d is %s, not validated", planID, version, status)
	}
	passed, detail, err = gate(ctx, p)
	if err != nil {
		return false, "", fmt.Errorf("plan: eval gate errored: %w", err)
	}
	if !passed {
		return false, detail, nil
	}
	return true, detail, l.transition(ctx, tenantID, planID, version, StatusValidated, StatusEvalPassed, nil)
}

// SignOff records the human governance decision (README task 8.4) and
// advances eval_passed -> signed_off.
func (l *Lifecycle) SignOff(ctx context.Context, tenantID, planID uuid.UUID, version int, by string) error {
	if by == "" {
		return fmt.Errorf("plan: sign-off requires a named signer")
	}
	if err := l.transition(ctx, tenantID, planID, version, StatusEvalPassed, StatusSignedOff, nil); err != nil {
		return err
	}
	return l.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE orchestration_plans SET signed_off_by = $3 WHERE plan_id = $1 AND version = $2`, planID, version, by)
		return err
	})
}

// Enable pins agentVersion/routeModelID into the spec and advances
// signed_off -> enabled — from here the row is immutable (task 8.4); a
// session that already started on this version keeps running against the
// spec it read at start, since Executor always loads by the EXACT
// (plan_id, plan_version) pinned onto its own session row, never "whatever
// is enabled now."
func (l *Lifecycle) Enable(ctx context.Context, tenantID, planID uuid.UUID, version, agentVersion int, routeModelID string) error {
	if err := l.transition(ctx, tenantID, planID, version, StatusSignedOff, StatusEnabled, func(p *Plan) {
		p.AgentVersion = agentVersion
		p.RouteModelID = routeModelID
	}); err != nil {
		return err
	}
	return l.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE orchestration_plans SET agent_version = $3, route_model_id = $4, enabled_at = now() WHERE plan_id = $1 AND version = $2`,
			planID, version, agentVersion, routeModelID)
		return err
	})
}

// Retire is the one transition an Enabled row still accepts.
func (l *Lifecycle) Retire(ctx context.Context, tenantID, planID uuid.UUID, version int) error {
	return l.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE orchestration_plans SET status = $3 WHERE plan_id = $1 AND version = $2 AND status = $4`,
			planID, version, StatusRetired, StatusEnabled)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("plan: %s version %d is not enabled; nothing to retire", planID, version)
		}
		return nil
	})
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
