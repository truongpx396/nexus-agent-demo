package promptctx

import (
	"fmt"
	"strings"

	"github.com/truongpx396/nexus-agent/internal/provider"
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
//
// The cut never separates a call from its results: a "tool" message holds the
// answers to the tool_use blocks of the assistant message before it, so if the
// first retained message would be one, the cut moves back to keep that
// assistant message with it. Otherwise the summary would replace the calls and
// the retained results would answer calls the model can no longer see, which a
// hosted API rejects.
func ShouldCondense(transcript []provider.Message, thresholdBytes int) (bool, int) {
	if thresholdBytes <= 0 {
		return false, 0
	}
	total := 0
	for _, m := range transcript {
		total += len(m.PlainText())
	}
	if total <= thresholdBytes {
		return false, 0
	}
	covered := len(transcript) - condenseKeepTail
	for covered > 0 && transcript[covered].Role == "tool" {
		covered--
	}
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
	n := len(transcript)
	if n == 0 {
		return ""
	}
	first, last := line(transcript[0]), line(transcript[n-1])
	switch n {
	case 1:
		return first
	case 2:
		return first + "\n" + last
	default:
		return fmt.Sprintf("%s\n[extractive: %d messages omitted]\n%s", first, n-2, last)
	}
}

func line(m provider.Message) string {
	text := m.PlainText()
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
// effect completed or failed, matching internal/eventlog.Condensation's own
// invariant (condensation_test.go) that a condensation can never answer
// whether an effect completed.
func CondensePrompt(transcript []provider.Message) provider.Prompt {
	var buf strings.Builder
	for _, m := range transcript {
		buf.WriteString(summaryLine(m))
		buf.WriteString("\n")
	}
	return provider.Prompt{
		System:   "Summarize the following conversation history concisely, preserving any decisions, facts, or open questions. Never state or imply that any external effect (a tool call, a send, a write) completed or failed — only summarize what was said.",
		Messages: []provider.Message{provider.TextMessage("user", buf.String())},
	}
}

// What the summariser is shown of one message. The 256-byte preview line() gives
// ExtractivePass is a fallback for when no model runs; the model asked to
// summarise a conversation needs the conversation, so text and a tool's result get
// 4 KiB each and a call shows what it was asked to do.
const (
	summaryTextBytes  = 4096
	summaryInputBytes = 512
)

// summaryLine renders one message for the summariser: its role, then each block on
// its own line. Reasoning is left out (it is round-tripped, never shown); a tool_use
// shows as "[called <tool> with <input>]", which PlainText leaves out altogether.
func summaryLine(m provider.Message) string {
	var parts []string
	for _, b := range m.Blocks {
		switch b.Kind {
		case provider.BlockText:
			parts = append(parts, cutBytes(b.Text, summaryTextBytes))
		case provider.BlockToolUse:
			parts = append(parts, fmt.Sprintf("[called %s with %s]", b.ToolName, cutBytes(string(b.Input), summaryInputBytes)))
		case provider.BlockToolResult:
			label := "[result]"
			if b.IsError {
				label = "[error result]"
			}
			parts = append(parts, label+" "+cutBytes(b.Text, summaryTextBytes))
		case provider.BlockThinking:
			// not part of the conversation
		}
	}
	return m.Role + ": " + strings.Join(parts, "\n")
}
