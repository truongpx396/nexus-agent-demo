// Package cost is the reserve-then-reconcile cost gate (docs/constitution.md,
// Principle IV: "stop on cost, not vibes"). Every model call is metered in
// exact integer currency micros — never float64, enforced package-wide by
// money_notfloat_test.go's AST guard (.golangci.yml's forbidigo comment
// explains why a lint pattern can't do this: it's a type ban, not a
// call-site ban) — reserved against a hard ceiling BEFORE the call is made,
// and reconciled against the real usage after.
package cost

import (
	"fmt"
	"strconv"
	"strings"
)

// Micros is the fixed scale every Money amount in this package is
// denominated in: one currency unit (e.g. one USD) is 1_000_000 Micros.
// Token prices are small fractions of a cent per token, so a scale this
// fine is what keeps per-token pricing exact rather than truncating to
// zero — the reason task 4.1 bans float64 in the first place: a float
// would silently absorb exactly this kind of sub-cent rounding.
const Micros int64 = 1_000_000

// Money is an exact integer amount in currency micros plus an explicit
// currency — no binary float anywhere in this package (README task 4.1).
// Two Money values only combine if their Currency matches; there is no
// implicit FX conversion (FX is explicitly out of scope, README §8).
type Money struct {
	Micros   int64
	Currency string
}

// Zero returns a zero amount in currency.
func Zero(currency string) Money { return Money{Currency: currency} }

// Add returns m+other. Both must share a currency.
func (m Money) Add(other Money) (Money, error) {
	if err := m.checkCurrency(other); err != nil {
		return Money{}, err
	}
	return Money{Micros: m.Micros + other.Micros, Currency: m.Currency}, nil
}

// Sub returns m-other. Both must share a currency.
func (m Money) Sub(other Money) (Money, error) {
	if err := m.checkCurrency(other); err != nil {
		return Money{}, err
	}
	return Money{Micros: m.Micros - other.Micros, Currency: m.Currency}, nil
}

// Cmp returns -1/0/1 as m is less than, equal to, or greater than other.
// Both must share a currency — a caller comparing two different
// currencies has already made an error the money type can catch instead
// of silently comparing incomparable numbers.
func (m Money) Cmp(other Money) (int, error) {
	if err := m.checkCurrency(other); err != nil {
		return 0, err
	}
	switch {
	case m.Micros < other.Micros:
		return -1, nil
	case m.Micros > other.Micros:
		return 1, nil
	default:
		return 0, nil
	}
}

func (m Money) checkCurrency(other Money) error {
	if m.Currency != other.Currency {
		return fmt.Errorf("cost: currency mismatch: %s vs %s", m.Currency, other.Currency)
	}
	return nil
}

// String renders m as a decimal amount (e.g. "0.050000 USD") for logs and
// audit detail strings — display formatting, never a computation input.
func (m Money) String() string {
	whole := m.Micros / Micros
	frac := m.Micros % Micros
	if frac < 0 {
		frac = -frac
	}
	return fmt.Sprintf("%d.%06d %s", whole, frac, m.Currency)
}

// ParseDecimal parses a plain decimal amount (e.g. "0.05", "12", "3.5") in
// currency units into exact Money micros, using only string/integer
// arithmetic — deliberately never strconv.ParseFloat: routing "0.05"
// through a binary float and scaling it back up is exactly the class of
// silent rounding drift this package's Money type exists to make
// impossible, even though the package's own AST guard
// (money_notfloat_test.go) only catches the literal float32/float64
// identifier, not this kind of indirect drift through an untyped literal.
// A caller-supplied ceiling (internal/surfaces/rest's handleCreateRun) is
// exactly the boundary where an external decimal string first becomes a
// Money value, so it is the one place this matters most.
func ParseDecimal(s, currency string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Money{}, fmt.Errorf("cost: empty decimal amount")
	}
	neg := false
	if after, ok := strings.CutPrefix(s, "-"); ok {
		neg = true
		s = after
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 6 {
		return Money{}, fmt.Errorf("cost: %q has more than 6 fractional digits (finer than the Micros scale)", s)
	}
	for len(frac) < 6 {
		frac += "0"
	}
	wholePart, err := strconv.ParseInt(whole, 10, 63)
	if err != nil {
		return Money{}, fmt.Errorf("cost: invalid decimal amount %q: %w", s, err)
	}
	fracPart, err := strconv.ParseInt(frac, 10, 63)
	if err != nil {
		return Money{}, fmt.Errorf("cost: invalid decimal amount %q: %w", s, err)
	}
	micros := wholePart*Micros + fracPart
	if neg {
		micros = -micros
	}
	return Money{Micros: micros, Currency: currency}, nil
}

// PriceQuantity computes the cost of quantity meter units (e.g. tokens)
// priced at pricePerMillionMicros — currency-micros per ONE MILLION units,
// the convention internal/cost/pricebook.go stores ("$3 per million
// tokens" is pricePerMillionMicros = 3_000_000): a per-token price this
// small would truncate to zero at Micros scale directly, so the price book
// quotes per-million instead and this function is the one place that
// divides back down, rounding ONCE, at this single asserted boundary
// (README task 4.1) — round-half-up on the final division, never
// accumulated across intermediate steps.
func PriceQuantity(quantity, pricePerMillionMicros int64, currency string) Money {
	if quantity == 0 || pricePerMillionMicros == 0 {
		return Zero(currency)
	}
	product := quantity * pricePerMillionMicros // currency-micros, still scaled up 1e6
	rounded := (product + 500_000) / 1_000_000
	return Money{Micros: rounded, Currency: currency}
}
