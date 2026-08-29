package cost

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// WildcardSubject is the price book subject that matches any priced
// subject (typically a model id) with no more specific entry — README task
// 4.3's "keyed (meter, priced subject, effective range)" allows a specific
// subject to override the wildcard without requiring every meter to be
// priced per model up front.
const WildcardSubject = "*"

// PriceBookEntry is one versioned price: how much one unit of Meter costs
// for Subject, over one effective range. Entries are never mutated once
// written — a price change is a NEW entry with a later EffectiveFrom and a
// closed EffectiveUntil on the one it supersedes, which is what keeps a
// historical cost reproducible (task 4.3) even after prices move.
type PriceBookEntry struct {
	ID                    uuid.UUID
	Meter                 MeterID
	Subject               string // a model id, or WildcardSubject
	Version               int
	Currency              string
	PricePerMillionMicros int64 // currency-micros per ONE MILLION meter units — see money.go's PriceQuantity
	EffectiveFrom         time.Time
	EffectiveUntil        *time.Time // nil = still in force
}

func (e PriceBookEntry) coversSubject(subject string) bool {
	return e.Subject == subject || e.Subject == WildcardSubject
}

func (e PriceBookEntry) coversTime(at time.Time) bool {
	if at.Before(e.EffectiveFrom) {
		return false
	}
	return e.EffectiveUntil == nil || at.Before(*e.EffectiveUntil)
}

// PriceBook is one tenant's loaded price book — an in-memory snapshot
// internal/cost/gate.go caches per tenant so Reserve/Reconcile's hot path
// never re-queries Postgres per call. Rebuilding it (a fresh LoadPriceBook)
// is how a Gate picks up a newly inserted entry.
type PriceBook struct {
	entries []PriceBookEntry
}

// NewPriceBook wraps a slice of entries already loaded from storage (or
// built by hand in a test) into a queryable PriceBook.
func NewPriceBook(entries []PriceBookEntry) *PriceBook {
	return &PriceBook{entries: entries}
}

// Lookup finds the entry that prices meter for subject at time at: the
// most specific subject match (an exact subject beats WildcardSubject),
// and among those, the highest version whose effective range covers at.
func (pb *PriceBook) Lookup(meter MeterID, subject string, at time.Time) (PriceBookEntry, bool) {
	var candidates []PriceBookEntry
	for _, e := range pb.entries {
		if e.Meter != meter || !e.coversSubject(subject) || !e.coversTime(at) {
			continue
		}
		candidates = append(candidates, e)
	}
	if len(candidates) == 0 {
		return PriceBookEntry{}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		// Exact subject match first, then highest version.
		iExact := candidates[i].Subject == subject
		jExact := candidates[j].Subject == subject
		if iExact != jExact {
			return iExact
		}
		return candidates[i].Version > candidates[j].Version
	})
	return candidates[0], true
}

// Cost prices quantity units of meter for subject at time at, using
// money.PriceQuantity's single rounding point. Returns an error if no
// price book entry covers this (meter, subject, at) — Reserve/Reconcile
// treat that as fail-closed (gate.go), never as free.
func (pb *PriceBook) Cost(meter MeterID, subject string, quantity int64, at time.Time) (Money, error) {
	entry, ok := pb.Lookup(meter, subject, at)
	if !ok {
		return Money{}, fmt.Errorf("cost: no price book entry for meter %q subject %q at %s", meter, subject, at)
	}
	return PriceQuantity(quantity, entry.PricePerMillionMicros, entry.Currency), nil
}

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
func LoadPriceBook(ctx context.Context, tx pgx.Tx) (*PriceBook, error) {
	rows, err := tx.Query(ctx, `
		SELECT price_book_id, meter, subject, version, currency,
		       price_micros_per_million, effective_from, effective_until
		FROM price_book`)
	if err != nil {
		return nil, fmt.Errorf("load price book: %w", err)
	}
	defer rows.Close()

	var entries []PriceBookEntry
	for rows.Next() {
		var e PriceBookEntry
		if err := rows.Scan(&e.ID, &e.Meter, &e.Subject, &e.Version, &e.Currency,
			&e.PricePerMillionMicros, &e.EffectiveFrom, &e.EffectiveUntil); err != nil {
			return nil, fmt.Errorf("scan price book entry: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load price book: %w", err)
	}
	return NewPriceBook(entries), nil
}

// InsertPriceBookEntry writes one new entry. Callers never UPDATE an
// existing row (entries are immutable once written, this file's own doc
// comment) — a price change is always a new INSERT.
func InsertPriceBookEntry(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e PriceBookEntry) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.Subject == "" {
		e.Subject = WildcardSubject
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
