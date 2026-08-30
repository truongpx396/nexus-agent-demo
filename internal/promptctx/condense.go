package promptctx

import (
	"fmt"
	"strings"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// condenseKeepTail is how many of the most recent messages ShouldCondense
// never offers up for condensation — the live edge of the conversation must
// never be summarized out from under the model mid-turn.
const condenseKeepTail = 4

// ShouldCondense reports whether transcript has grown past thresholdBytes —
// byte length is the declared, measured proxy for "~80% of budget" (task
// 7.11); this codebase has no tokenizer (internal/promptctx/cache.go's own
// CacheReadRate works from provider-reported usage figures, not a local
// count), so a byte-length approximation is the honest choice, not a true
// token count. thresholdBytes<=0 disables condensation entirely — the
// pre-Phase-7 default every earlier caller/test still gets. When true,
// covered is how many of the OLDEST messages a condensation would cover.
func ShouldCondense(transcript []provider.Message, thresholdBytes int) (bool, int) {
	if thresholdBytes <= 0 {
		return false, 0
	}
	total := 0
	for _, m := range transcript {
		total += len(m.Text)
	}
	if total <= thresholdBytes {
		return false, 0
	}
	covered := len(transcript) - condenseKeepTail
	if covered <= 0 {
		return false, 0
	}
	return true, covered
}

// ExtractivePass is the no-model degrade path (README tasks 7.2 and 7.11):
// deterministic, never calls a model. Keeps the first and last message of
// the covered range verbatim and collapses everything between them to a
// count — enough to preserve the shape of what was condensed without an
// LLM call, for when the compaction budget reservation is refused.
func ExtractivePass(transcript []provider.Message) string {
	switch len(transcript) {
	case 0:
		return ""
	case 1:
		return line(transcript[0])
	case 2:
		return line(transcript[0]) + "\n" + line(transcript[1])
	default:
		return fmt.Sprintf("%s\n[extractive: %d messages omitted]\n%s",
			line(transcript[0]), len(transcript)-2, line(transcript[len(transcript)-1]))
	}
}

func line(m provider.Message) string {
	text := m.Text
	if len(text) > previewBytes {
		text = text[:previewBytes]
	}
	return m.Role + ": " + text
}

// CondensePrompt builds the small summarization prompt kernel feeds to
// Provider.Stream under a cheaper model id (kernel.RunConfig.
// CondenserModelID) — the same Provider port every other call goes through
// (task 7.11: "off the paying loop" means a cheaper metered call, never an
// unmetered one). The instruction explicitly forbids claiming an external
// effect completed or failed, matching internal/store.Condensation's own
// invariant (condensation_test.go) that a condensation can never answer
// whether an effect completed.
func CondensePrompt(transcript []provider.Message) provider.Prompt {
	var buf strings.Builder
	for _, m := range transcript {
		buf.WriteString(line(m))
		buf.WriteString("\n")
	}
	return provider.Prompt{
		System: "Summarize the following conversation history concisely, preserving any decisions, facts, or open questions. Never state or imply that any external effect (a tool call, a send, a write) completed or failed — only summarize what was said.",
		Messages: []provider.Message{
			{Role: "user", Text: buf.String()},
		},
	}
}
