package cost

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CostRecord is one meter's actual, reconciled cost for one model call —
// the durable truth Reconcile writes AFTER the call, as opposed to
// Decision's pre-spend estimate. Multiple CostRecords (one per meter with
// nonzero quantity) back a single Reconcile call, all inserted together
// (RecordUsage below).
type CostRecord struct {
	ID            uuid.UUID
	ReservationID *uuid.UUID // the Reservation this reconciles, nil for a direct Record call
	Meter         MeterID
	Quantity      int64
	Unit          string
	Cost          Money
	ModelID       string
	Unreported    bool // true when usage was UNREPORTED and this is the reserved worst-case, not measured usage (task 4.7)
}

// RecordUsage durably inserts one or more CostRecords in a single
// statement batch — "cost records appended ... " (README task 4.9); see
// gate.go's doc comment on Gate for why this runs in the Gate's own
// transaction rather than literally sharing kernel's per-event transaction.
func RecordUsage(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, records []CostRecord) error {
	for _, r := range records {
		if r.ID == uuid.Nil {
			r.ID = uuid.New()
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO cost_records (
				cost_record_id, tenant_id, session_id, reservation_id,
				meter, quantity, unit, minor_units, currency, model_id, unreported
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			r.ID, tenantID, sessionID, r.ReservationID,
			string(r.Meter), r.Quantity, r.Unit, r.Cost.Micros, currencyOrDefault(r.Cost.Currency), nullIfEmpty(r.ModelID), r.Unreported,
		)
		if err != nil {
			return fmt.Errorf("record cost usage (meter=%s): %w", r.Meter, err)
		}
	}
	return nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// SumCostRecords totals every recorded cost, in micros, against budgetScope
// — used to rehydrate a fresh Redis epoch counter from Postgres's own
// durable history (gate.go's Arm) rather than ever assuming zero spend on
// a cold cache (README task 4.4's "never 'no spend yet'"). For
// BudgetScopeTenant this sums across every session in the tenant; for
// BudgetScopeSession it sums just sessionID's own rows.
func SumCostRecords(ctx context.Context, tx pgx.Tx, scope BudgetScope, sessionID *uuid.UUID) (int64, error) {
	var total *int64
	var err error
	if scope == BudgetScopeSession {
		err = tx.QueryRow(ctx, `SELECT sum(minor_units) FROM cost_records WHERE session_id = $1`, sessionID).Scan(&total)
	} else {
		err = tx.QueryRow(ctx, `SELECT sum(minor_units) FROM cost_records`).Scan(&total)
	}
	if err != nil {
		return 0, fmt.Errorf("sum cost records: %w", err)
	}
	if total == nil {
		return 0, nil
	}
	return *total, nil
}
