package cost

import "testing"

func TestMoney_AddSub(t *testing.T) {
	a := Money{Micros: 300, Currency: "USD"}
	b := Money{Micros: 200, Currency: "USD"}

	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sum.Micros != 500 {
		t.Fatalf("Add = %d, want 500", sum.Micros)
	}

	diff, err := a.Sub(b)
	if err != nil {
		t.Fatalf("Sub: %v", err)
	}
	if diff.Micros != 100 {
		t.Fatalf("Sub = %d, want 100", diff.Micros)
	}
}

func TestMoney_CurrencyMismatchIsRefused(t *testing.T) {
	usd := Money{Micros: 100, Currency: "USD"}
	eur := Money{Micros: 100, Currency: "EUR"}

	if _, err := usd.Add(eur); err == nil {
		t.Fatal("Add across currencies succeeded, want an error")
	}
	if _, err := usd.Sub(eur); err == nil {
		t.Fatal("Sub across currencies succeeded, want an error")
	}
	if _, err := usd.Cmp(eur); err == nil {
		t.Fatal("Cmp across currencies succeeded, want an error")
	}
}

func TestMoney_Cmp(t *testing.T) {
	low := Money{Micros: 100, Currency: "USD"}
	high := Money{Micros: 200, Currency: "USD"}

	if got, _ := low.Cmp(high); got != -1 {
		t.Fatalf("low.Cmp(high) = %d, want -1", got)
	}
	if got, _ := high.Cmp(low); got != 1 {
		t.Fatalf("high.Cmp(low) = %d, want 1", got)
	}
	if got, _ := low.Cmp(low); got != 0 {
		t.Fatalf("low.Cmp(low) = %d, want 0", got)
	}
}

func TestPriceQuantity_RoundsHalfUpOnce(t *testing.T) {
	// $3 per million tokens, 7 tokens: 7*3_000_000/1_000_000 = 21 micros exactly.
	got := PriceQuantity(7, 3_000_000, "USD")
	if got.Micros != 21 {
		t.Fatalf("got %d micros, want 21", got.Micros)
	}

	// A case that lands on a genuine half: 1 unit at 500_000 micros/million
	// = 0.5 micros, rounds up to 1.
	got = PriceQuantity(1, 500_000, "USD")
	if got.Micros != 1 {
		t.Fatalf("got %d micros, want 1 (round-half-up)", got.Micros)
	}
}

func TestPriceQuantity_ZeroQuantityOrPriceIsZero(t *testing.T) {
	if got := PriceQuantity(0, 3_000_000, "USD"); got.Micros != 0 {
		t.Fatalf("zero quantity: got %d, want 0", got.Micros)
	}
	if got := PriceQuantity(100, 0, "USD"); got.Micros != 0 {
		t.Fatalf("zero price: got %d, want 0", got.Micros)
	}
}

func TestParseDecimal(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0.05", 50_000},
		{"12", 12_000_000},
		{"3.5", 3_500_000},
		{"0.000001", 1},
		{"-1.25", -1_250_000},
	}
	for _, c := range cases {
		got, err := ParseDecimal(c.in, "USD")
		if err != nil {
			t.Fatalf("ParseDecimal(%q): %v", c.in, err)
		}
		if got.Micros != c.want {
			t.Errorf("ParseDecimal(%q) = %d micros, want %d", c.in, got.Micros, c.want)
		}
		if got.Currency != "USD" {
			t.Errorf("ParseDecimal(%q) currency = %q, want USD", c.in, got.Currency)
		}
	}
}

func TestParseDecimal_Invalid(t *testing.T) {
	for _, in := range []string{"", "abc", "1.2345678", "1.2.3"} {
		if _, err := ParseDecimal(in, "USD"); err == nil {
			t.Errorf("ParseDecimal(%q) succeeded, want an error", in)
		}
	}
}

func TestMoney_String(t *testing.T) {
	m := Money{Micros: 50_000, Currency: "USD"}
	if got, want := m.String(), "0.050000 USD"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
