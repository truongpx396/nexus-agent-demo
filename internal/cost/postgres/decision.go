package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
)

// RecordDecision inserts one durable audit row into budget_decisions.
// gate.go calls this from within Reserve, in the Gate's own transaction —
// the corresponding eventlog.EventBudgetDecision is a separate append the
// caller (kernel) makes into the event log itself; internal/cost never
// writes to the events table directly (store.Append is the log's one
// sanctioned writer, docs/constitution.md Principle II).
func RecordDecision(ctx context.Context, tx pgx.Tx, tenantID, sessionID uuid.UUID, d cost.Decision) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO budget_decisions (
			budget_decision_id, tenant_id, session_id, decision, reason,
			budget_id, reserved_micros, currency
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		uuid.New(), tenantID, sessionID, string(d.Kind), d.Reason,
		d.BudgetID, d.Reserved.Micros, currencyOrDefault(d.Reserved.Currency),
	)
	if err != nil {
		return fmt.Errorf("record budget decision: %w", err)
	}
	return nil
}

func currencyOrDefault(c string) string {
	if c == "" {
		return cost.DefaultCurrency
	}
	return c
}
