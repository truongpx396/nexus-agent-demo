package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
)

// --- Postgres access (migrations/0005_cost.sql) ---
//
// These take a caller-supplied tx, exactly like internal/crypto.KeyStore's
// methods (that package's own doc comment: "every method must run inside a
// tenant-scoped transaction") — price_book carries tenant_id and RLS like
// every other table (README §4's blanket rule), even though task 4.3's
// per-tenant price OVERRIDE feature is explicitly out of scope (README §3,
// pattern 65): every tenant is seeded with the same entries (cmd/nexusd's
// seed command), and the column exists for schema consistency, not because
// a tenant can configure its own prices yet.

// LoadPriceBook reads every price_book row visible to tx's tenant scope.
func LoadPriceBook(ctx context.Context, tx pgx.Tx) (*cost.PriceBook, error) {
	rows, err := tx.Query(ctx, `
		SELECT price_book_id, meter, subject, version, currency,
		       price_micros_per_million, effective_from, effective_until
		FROM price_book`)
	if err != nil {
		return nil, fmt.Errorf("load price book: %w", err)
	}
	defer rows.Close()

	var entries []cost.PriceBookEntry
	for rows.Next() {
		var e cost.PriceBookEntry
		if err := rows.Scan(&e.ID, &e.Meter, &e.Subject, &e.Version, &e.Currency,
			&e.PricePerMillionMicros, &e.EffectiveFrom, &e.EffectiveUntil); err != nil {
			return nil, fmt.Errorf("scan price book entry: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load price book: %w", err)
	}
	return cost.NewPriceBook(entries), nil
}

// InsertPriceBookEntry writes one new entry. Callers never UPDATE an
// existing row (entries are immutable once written, this file's own doc
// comment) — a price change is always a new INSERT.
func InsertPriceBookEntry(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e cost.PriceBookEntry) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.Subject == "" {
		e.Subject = cost.WildcardSubject
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO price_book (
			price_book_id, tenant_id, meter, subject, version, currency,
			price_micros_per_million, effective_from, effective_until
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		e.ID, tenantID, string(e.Meter), e.Subject, e.Version, e.Currency,
		e.PricePerMillionMicros, e.EffectiveFrom, e.EffectiveUntil,
	)
	if err != nil {
		return fmt.Errorf("insert price book entry: %w", err)
	}
	return nil
}
