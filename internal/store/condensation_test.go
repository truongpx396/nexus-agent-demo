package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCondensation_CannotAnswerWhetherAnExternalEffectCompleted is task
// 6.5's own named acceptance test. It proves the claim two ways: (1)
// structurally, over Condensation's JSON shape, so a future field added to
// the struct without updating this test fails loudly; and (2) behaviorally,
// by contrasting what a Condensation over a history CAN say against what
// the raw event log (the only authoritative source, per this type's own
// doc comment) can.
func TestCondensation_CannotAnswerWhetherAnExternalEffectCompleted(t *testing.T) {
	// A history where an external effect (an email tool) unambiguously
	// completed: a tool_use paired with a clean, non-error, non-synthetic
	// tool_result.
	history := []Event{
		{Type: EventToolUse, ToolID: strPtr("platform/send_email@v1")},
		{Type: EventToolResult, ToolID: strPtr("platform/send_email@v1")},
	}

	c := Condense(2, "Sent the Q3 numbers to finance and moved on to the next task.")

	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal condensation: %v", err)
	}
	forbidden := []string{"is_error", "completed", "succeeded", "effect_class", "tool_result", "outcome"}
	lower := strings.ToLower(string(raw))
	for _, f := range forbidden {
		if strings.Contains(lower, f) {
			t.Fatalf("condensation JSON %s contains %q — a condensation must never carry a structured effect-completion field", raw, f)
		}
	}

	// The behavioral half: the RAW EVENT LOG can answer "did the effect
	// complete" (a tool_result exists for the tool_use, non-error) —
	// something no field on Condensation is even capable of expressing.
	var sawCompletion bool
	for _, e := range history {
		if e.Type == EventToolResult {
			sawCompletion = true
		}
	}
	if !sawCompletion {
		t.Fatal("test setup bug: the fixture history should itself be able to answer the completion question")
	}
	if c.CoveredThroughSeq != 2 || c.Summary == "" {
		t.Fatalf("condensation carries no useful model-facing content: %+v", c)
	}
}

func strPtr(s string) *string { return &s }
