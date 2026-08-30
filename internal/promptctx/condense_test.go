package promptctx

import (
	"strings"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

func TestShouldCondense_DisabledByZeroThreshold(t *testing.T) {
	transcript := []provider.Message{{Role: "tool", Text: strings.Repeat("x", 1_000_000)}}
	ok, covered := ShouldCondense(transcript, 0)
	if ok || covered != 0 {
		t.Errorf("ShouldCondense with thresholdBytes<=0 = (%v, %d), want (false, 0)", ok, covered)
	}
}

func TestShouldCondense_TriggersPastThreshold(t *testing.T) {
	transcript := make([]provider.Message, 10)
	for i := range transcript {
		transcript[i] = provider.Message{Role: "tool", Text: strings.Repeat("x", 100)}
	}
	ok, covered := ShouldCondense(transcript, 500)
	if !ok {
		t.Fatal("ShouldCondense = false, want true for a transcript well past the threshold")
	}
	if covered != len(transcript)-condenseKeepTail {
		t.Errorf("covered = %d, want %d (len - condenseKeepTail)", covered, len(transcript)-condenseKeepTail)
	}
}

func TestShouldCondense_BelowThresholdDoesNotTrigger(t *testing.T) {
	transcript := []provider.Message{{Role: "tool", Text: "short"}}
	ok, _ := ShouldCondense(transcript, 1_000_000)
	if ok {
		t.Error("ShouldCondense = true for a transcript well under the threshold")
	}
}

func TestExtractivePass_DeterministicAndNeverCallsAModel(t *testing.T) {
	transcript := []provider.Message{
		{Role: "user", Text: "first message"},
		{Role: "assistant", Text: "middle one"},
		{Role: "tool", Text: "middle two"},
		{Role: "assistant", Text: "last message"},
	}
	out1 := ExtractivePass(transcript)
	out2 := ExtractivePass(transcript)
	if out1 != out2 {
		t.Errorf("ExtractivePass is not deterministic: %q vs %q", out1, out2)
	}
	if !strings.Contains(out1, "first message") || !strings.Contains(out1, "last message") {
		t.Errorf("ExtractivePass = %q, want the first and last message kept verbatim", out1)
	}
	if !strings.Contains(out1, "omitted") {
		t.Errorf("ExtractivePass = %q, want a count of omitted messages", out1)
	}
}

func TestExtractivePass_EmptyAndSingleton(t *testing.T) {
	if got := ExtractivePass(nil); got != "" {
		t.Errorf("ExtractivePass(nil) = %q, want empty", got)
	}
	one := []provider.Message{{Role: "user", Text: "only message"}}
	if got := ExtractivePass(one); !strings.Contains(got, "only message") {
		t.Errorf("ExtractivePass(one message) = %q, want it to contain the message", got)
	}
}

func TestCondensePrompt_InstructsAgainstClaimingAnEffectOutcome(t *testing.T) {
	// This is an instruction TO the condenser model, so it necessarily
	// names "completed"/"failed" — what matters is that it forbids
	// claiming either, matching internal/store.Condensation's own
	// invariant (condensation_test.go) that a condensation can never
	// answer whether an external effect completed.
	transcript := []provider.Message{{Role: "tool", Text: "the file was written"}}
	prompt := CondensePrompt(transcript)
	lower := strings.ToLower(prompt.System)
	if !strings.Contains(lower, "never") || !strings.Contains(lower, "completed") {
		t.Errorf("CondensePrompt.System = %q, want an explicit instruction against claiming an effect completed", prompt.System)
	}
	if len(prompt.Messages) != 1 || prompt.Messages[0].Role != "user" {
		t.Errorf("CondensePrompt.Messages = %+v, want exactly one user message", prompt.Messages)
	}
}
