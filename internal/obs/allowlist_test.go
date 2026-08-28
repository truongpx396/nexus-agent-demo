package obs

import "testing"

// contentBearingKeys is every key a real span attribute set is tempted to
// carry, sourced from README.md's own list of what must never appear on a
// span: "prompt text, model output, tool arguments and results, memory,
// retrieved documents, and approval context packages". This test tries all
// of them, plus a few obvious variants, and asserts every single one is
// dropped — not truncated, not redacted, dropped.
func TestFilterDropsEveryContentBearingKey(t *testing.T) {
	contentBearingKeys := []string{
		"prompt",
		"content",
		"thought",
		"tool_input",
		"tool_output",
		"tool.input",
		"tool.output",
		"tool.args",
		"memory.entry",
		"memory_entry",
		"retrieved_document",
		"approval.context",
		"approval_context",
		"steering_message",
		"input_answer",
		"input_request.body",
		"delegation_goal",
		"delegation_summary",
		"error.message", // free-text provider errors must be reduced to typed classes first
		"raw_response",
		"system_prompt",
	}

	for _, key := range contentBearingKeys {
		t.Run(key, func(t *testing.T) {
			if IsAllowed(key) {
				t.Fatalf("content-bearing key %q is allowlisted — this must never happen", key)
			}
			out := Filter(Attrs{key: "some content that must never reach the exporter"})
			if _, present := out[key]; present {
				t.Fatalf("Filter let %q through", key)
			}
			if len(out) != 0 {
				t.Fatalf("Filter produced unexpected keys: %v", out)
			}
		})
	}
}

func TestFilterKeepsStructuralKeys(t *testing.T) {
	structuralKeys := []string{
		"session.id", "tenant.id", "tool.id", "terminal_reason",
		"model.id", "usage.input_cache_read", "latency_ms",
	}

	in := make(Attrs, len(structuralKeys))
	for _, k := range structuralKeys {
		in[k] = "v"
	}

	out := Filter(in)
	if len(out) != len(structuralKeys) {
		t.Fatalf("Filter dropped a structural key: got %d keys, want %d (%v)", len(out), len(structuralKeys), out)
	}
	for _, k := range structuralKeys {
		if _, ok := out[k]; !ok {
			t.Errorf("Filter dropped structural key %q", k)
		}
	}
}

func TestFilterMixedInputOnlyKeepsAllowlisted(t *testing.T) {
	in := Attrs{
		"session.id": "s-1",
		"content":    "leaked?",
	}
	out := Filter(in)
	if len(out) != 1 {
		t.Fatalf("Filter should keep exactly 1 key, got %d: %v", len(out), out)
	}
	if out["session.id"] != "s-1" {
		t.Fatalf("Filter changed the value of an allowlisted key: %v", out)
	}
	if _, present := out["content"]; present {
		t.Fatal("Filter let a content key through in a mixed attribute set")
	}
}
