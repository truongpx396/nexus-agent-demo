package main

import (
	"fmt"
	"time"

	"github.com/truongpx396/nexus-agent/internal/cost"
)

// modelPrice is one model's rates in currency-micros per million tokens.
type modelPrice struct {
	model                  string
	inputPerMillionMicros  int64
	outputPerMillionMicros int64
}

// routedModelPrices are the three models internal/provider/router.go names. The
// real rates differ by 5x between Haiku and Opus, so each is priced on its own
// (README task 13.6, closing F8).
var routedModelPrices = []modelPrice{
	{"claude-haiku-4-5", 1_000_000, 5_000_000}, // $1.00 in / $5.00 out per million tokens
	{"claude-sonnet-5", 2_000_000, 10_000_000}, // $2.00 in / $10.00 out per million tokens
	{"claude-opus-5", 5_000_000, 25_000_000},   // $5.00 in / $25.00 out per million tokens
}

// boundModelPrice is the model the configured provider will actually call, when
// the three above do not already price it.
//
// A run is priced under the model its provider is bound to, not under the one the
// router chose (provider.ServedModel), because nothing in a model call carries
// the router's choice. So that model needs price entries too:
//
//   - LiteLLM serves a local model. It is seeded at zero, which is the real rate
//     of a model on the operator's own hardware. If LiteLLM fronts a paid model,
//     insert its real rates instead.
//   - Anthropic with a NEXUS_ANTHROPIC_MODEL that is not one of the three has no
//     price this command knows. It warns, and Reserve then refuses the run rather
//     than guess: an unpriced model must never look free.
func boundModelPrice(providerName, model string) (p modelPrice, warning string, ok bool) {
	switch providerName {
	case "litellm":
		return modelPrice{model: model}, "", true
	case "anthropic":
		for _, known := range routedModelPrices {
			if known.model == model {
				return modelPrice{}, "", false
			}
		}
		return modelPrice{}, fmt.Sprintf("NEXUS_ANTHROPIC_MODEL=%s has no price in the seed. Insert price_book entries for it, or every run will be refused for want of a price", model), false
	}
	return modelPrice{}, "", false
}

// seedPriceEntries lists every entry a tenant should have, in the order they are
// inserted, plus a warning for the operator when the configured model cannot be
// priced.
func seedPriceEntries(now time.Time) (entries []cost.PriceBookEntry, warning string) {
	models := append([]modelPrice(nil), routedModelPrices...)
	switch name := envOr("NEXUS_PROVIDER", "fake"); name {
	case "litellm":
		if p, _, ok := boundModelPrice(name, envOr("NEXUS_LITELLM_MODEL", defaultLiteLLMModel)); ok {
			models = append(models, p)
		}
	case "anthropic":
		_, warning, _ = boundModelPrice(name, envOr("NEXUS_ANTHROPIC_MODEL", defaultAnthropicModel))
	}

	for _, m := range models {
		for _, e := range []struct {
			meter cost.MeterID
			price int64
		}{
			{cost.MeterInputUncached, m.inputPerMillionMicros},
			{cost.MeterInputCacheWrite, m.inputPerMillionMicros * 5 / 4}, // 1.25x the model's own input rate
			{cost.MeterInputCacheRead, m.inputPerMillionMicros / 10},     // 0.1x the model's own input rate
			{cost.MeterOutput, m.outputPerMillionMicros},
		} {
			entries = append(entries, cost.PriceBookEntry{
				Meter: e.meter, Subject: m.model, Version: 1,
				Currency: cost.DefaultCurrency, PricePerMillionMicros: e.price, EffectiveFrom: now,
			})
		}
	}

	// Embeddings keep one wildcard entry: the retrieval tier always embeds under
	// "fake-embedder-v1", never one of the chat models.
	entries = append(entries, cost.PriceBookEntry{
		Meter: cost.MeterEmbeddingTokens, Subject: cost.WildcardSubject, Version: 1,
		Currency: cost.DefaultCurrency, PricePerMillionMicros: 100_000, EffectiveFrom: now, // $0.10 per million embedding tokens
	})
	return entries, warning
}
