package main

import (
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/nexus-agent/internal/cost"
)

func entriesFor(entries []cost.PriceBookEntry, model string) map[cost.MeterID]int64 {
	out := map[cost.MeterID]int64{}
	for _, e := range entries {
		if e.Subject == model {
			out[e.Meter] = e.PricePerMillionMicros
		}
	}
	return out
}

func TestSeedPriceEntries_PricesTheThreeRoutedModelsAtTheirOwnRates(t *testing.T) {
	t.Setenv("NEXUS_PROVIDER", "fake")
	entries, warning := seedPriceEntries(time.Now())
	if warning != "" {
		t.Fatalf("warning = %q, want none", warning)
	}
	opus := entriesFor(entries, "claude-opus-5")
	if opus[cost.MeterInputUncached] != 5_000_000 || opus[cost.MeterOutput] != 25_000_000 ||
		opus[cost.MeterInputCacheWrite] != 6_250_000 || opus[cost.MeterInputCacheRead] != 500_000 {
		t.Fatalf("opus rates = %v, want 5/25 per million with the 1.25x and 0.1x cache multipliers", opus)
	}
	if got := entriesFor(entries, "claude-haiku-4-5")[cost.MeterOutput]; got != 5_000_000 {
		t.Fatalf("haiku output rate = %d, want 5_000_000", got)
	}
	if len(entriesFor(entries, cost.WildcardSubject)) != 1 {
		t.Fatalf("want exactly the one wildcard embedding entry, got %v", entriesFor(entries, cost.WildcardSubject))
	}
}

// A run is priced under the model its provider is bound to, so that model must
// have entries. A local model through LiteLLM is seeded at zero, or every local
// run would be refused for want of a price.
func TestSeedPriceEntries_APricesTheLocalModelBehindLiteLLMAtZero(t *testing.T) {
	t.Setenv("NEXUS_PROVIDER", "litellm")
	t.Setenv("NEXUS_LITELLM_MODEL", "qwen-test")
	entries, _ := seedPriceEntries(time.Now())
	got := entriesFor(entries, "qwen-test")
	for _, m := range []cost.MeterID{cost.MeterInputUncached, cost.MeterInputCacheWrite, cost.MeterInputCacheRead, cost.MeterOutput} {
		if rate, ok := got[m]; !ok || rate != 0 {
			t.Fatalf("meter %s for the local model = %d (present %v), want a zero entry", m, rate, ok)
		}
	}
}

func TestSeedPriceEntries_AnUnpricedAnthropicModelWarnsAndIsNotGuessed(t *testing.T) {
	t.Setenv("NEXUS_PROVIDER", "anthropic")
	t.Setenv("NEXUS_ANTHROPIC_MODEL", "claude-unknown-9")
	entries, warning := seedPriceEntries(time.Now())
	if !strings.Contains(warning, "claude-unknown-9") {
		t.Fatalf("warning = %q, want it to name the unpriced model", warning)
	}
	if got := entriesFor(entries, "claude-unknown-9"); len(got) != 0 {
		t.Fatalf("an unknown model was given a price: %v", got)
	}
}

func TestSeedPriceEntries_APricedAnthropicModelNeedsNoWarning(t *testing.T) {
	t.Setenv("NEXUS_PROVIDER", "anthropic")
	t.Setenv("NEXUS_ANTHROPIC_MODEL", "")
	if _, warning := seedPriceEntries(time.Now()); warning != "" {
		t.Fatalf("the default model is priced, but warning = %q", warning)
	}
}
