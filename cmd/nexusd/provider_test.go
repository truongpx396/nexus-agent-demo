package main

import "testing"

func TestAnthropicMaxTokens(t *testing.T) {
	for name, tc := range map[string]struct {
		env  string
		want int
	}{
		"unset":         {"", defaultAnthropicMaxTokens},
		"valid":         {"32000", 32000},
		"the floor":     {"1024", 1024},
		"the ceiling":   {"128000", 128000},
		"too small":     {"1023", defaultAnthropicMaxTokens},
		"too large":     {"128001", defaultAnthropicMaxTokens},
		"negative":      {"-5", defaultAnthropicMaxTokens},
		"not a number":  {"lots", defaultAnthropicMaxTokens},
		"a decimal":     {"16000.5", defaultAnthropicMaxTokens},
		"zero":          {"0", defaultAnthropicMaxTokens},
		"with a suffix": {"16k", defaultAnthropicMaxTokens},
	} {
		t.Setenv("NEXUS_ANTHROPIC_MAX_TOKENS", tc.env)
		if got := anthropicMaxTokens(); got != tc.want {
			t.Errorf("%s: anthropicMaxTokens() = %d, want %d", name, got, tc.want)
		}
	}
}

// The gate's worst-case output reservation has to cover what the provider may
// produce, or a call that runs to its limit overshoots it.
func TestCostGateConfigSizesTheOutputReservationFromTheAnthropicLimit(t *testing.T) {
	t.Setenv("NEXUS_ANTHROPIC_MAX_TOKENS", "32000")

	t.Setenv("NEXUS_PROVIDER", "anthropic")
	if got := costGateConfig().MaxOutputTokenEstimate; got != 32000 {
		t.Errorf("anthropic: MaxOutputTokenEstimate = %d, want 32000", got)
	}
	for _, other := range []string{"fake", "litellm", ""} {
		t.Setenv("NEXUS_PROVIDER", other)
		if got := costGateConfig().MaxOutputTokenEstimate; got != 0 {
			t.Errorf("provider %q: MaxOutputTokenEstimate = %d, want the gate's own default (0 here)", other, got)
		}
	}
}
