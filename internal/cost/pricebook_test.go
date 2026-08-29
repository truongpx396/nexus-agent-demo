package cost

import (
	"testing"
	"time"
)

func TestPriceBook_ExactSubjectBeatsWildcard(t *testing.T) {
	now := time.Now()
	pb := NewPriceBook([]PriceBookEntry{
		{Meter: MeterOutput, Subject: WildcardSubject, Version: 1, Currency: "USD", PricePerMillionMicros: 15_000_000, EffectiveFrom: now.Add(-time.Hour)},
		{Meter: MeterOutput, Subject: "claude-haiku-4-5", Version: 1, Currency: "USD", PricePerMillionMicros: 4_000_000, EffectiveFrom: now.Add(-time.Hour)},
	})

	entry, ok := pb.Lookup(MeterOutput, "claude-haiku-4-5", now)
	if !ok {
		t.Fatal("Lookup did not find the exact-subject entry")
	}
	if entry.PricePerMillionMicros != 4_000_000 {
		t.Fatalf("got price %d, want the exact-subject entry's 4_000_000", entry.PricePerMillionMicros)
	}

	entry, ok = pb.Lookup(MeterOutput, "some-other-model", now)
	if !ok {
		t.Fatal("Lookup did not fall back to the wildcard entry")
	}
	if entry.PricePerMillionMicros != 15_000_000 {
		t.Fatalf("got price %d, want the wildcard entry's 15_000_000", entry.PricePerMillionMicros)
	}
}

func TestPriceBook_HighestVersionWins(t *testing.T) {
	now := time.Now()
	pb := NewPriceBook([]PriceBookEntry{
		{Meter: MeterOutput, Subject: WildcardSubject, Version: 1, Currency: "USD", PricePerMillionMicros: 15_000_000, EffectiveFrom: now.Add(-2 * time.Hour)},
		{Meter: MeterOutput, Subject: WildcardSubject, Version: 2, Currency: "USD", PricePerMillionMicros: 20_000_000, EffectiveFrom: now.Add(-time.Hour)},
	})

	entry, ok := pb.Lookup(MeterOutput, WildcardSubject, now)
	if !ok {
		t.Fatal("Lookup found nothing")
	}
	if entry.Version != 2 || entry.PricePerMillionMicros != 20_000_000 {
		t.Fatalf("got version %d price %d, want version 2 price 20_000_000", entry.Version, entry.PricePerMillionMicros)
	}
}

func TestPriceBook_RespectsEffectiveRange(t *testing.T) {
	now := time.Now()
	until := now.Add(-time.Minute) // expired one minute ago
	pb := NewPriceBook([]PriceBookEntry{
		{Meter: MeterOutput, Subject: WildcardSubject, Version: 1, Currency: "USD", PricePerMillionMicros: 15_000_000, EffectiveFrom: now.Add(-2 * time.Hour), EffectiveUntil: &until},
	})

	if _, ok := pb.Lookup(MeterOutput, WildcardSubject, now); ok {
		t.Fatal("Lookup returned an entry outside its effective range")
	}

	// Still valid a moment before it expired.
	beforeExpiry := until.Add(-time.Second)
	if _, ok := pb.Lookup(MeterOutput, WildcardSubject, beforeExpiry); !ok {
		t.Fatal("Lookup should find the entry before its EffectiveUntil")
	}
}

func TestPriceBook_Cost_NoEntryIsAnError(t *testing.T) {
	pb := NewPriceBook(nil)
	if _, err := pb.Cost(MeterOutput, WildcardSubject, 100, time.Now()); err == nil {
		t.Fatal("Cost with an empty price book succeeded, want an error (fail closed, never free)")
	}
}

func TestPriceBook_Cost(t *testing.T) {
	now := time.Now()
	pb := NewPriceBook([]PriceBookEntry{
		{Meter: MeterOutput, Subject: WildcardSubject, Version: 1, Currency: "USD", PricePerMillionMicros: 15_000_000, EffectiveFrom: now.Add(-time.Hour)},
	})
	got, err := pb.Cost(MeterOutput, WildcardSubject, 1_000, now)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	// 1000 tokens * 15_000_000 micros/million = 15_000 micros exactly.
	if got.Micros != 15_000 {
		t.Fatalf("got %d micros, want 15_000", got.Micros)
	}
}
