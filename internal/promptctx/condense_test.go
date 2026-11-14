package promptctx

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

func TestShouldCondense_DisabledByZeroThreshold(t *testing.T) {
	transcript := []provider.Message{provider.TextMessage("tool", strings.Repeat("x", 1_000_000))}
	ok, covered := ShouldCondense(transcript, 0)
	if ok || covered != 0 {
		t.Errorf("ShouldCondense with thresholdBytes<=0 = (%v, %d), want (false, 0)", ok, covered)
	}
}

func TestShouldCondense_TriggersPastThreshold(t *testing.T) {
	transcript := make([]provider.Message, 10)
	for i := range transcript {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		transcript[i] = provider.TextMessage(role, strings.Repeat("x", 100))
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
	transcript := []provider.Message{provider.TextMessage("tool", "short")}
	ok, _ := ShouldCondense(transcript, 1_000_000)
	if ok {
		t.Error("ShouldCondense = true for a transcript well under the threshold")
	}
}

func TestExtractivePass_DeterministicAndNeverCallsAModel(t *testing.T) {
	transcript := []provider.Message{
		provider.TextMessage("user", "first message"),
		provider.TextMessage("assistant", "middle one"),
		provider.TextMessage("tool", "middle two"),
		provider.TextMessage("assistant", "last message"),
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
	one := []provider.Message{provider.TextMessage("user", "only message")}
	if got := ExtractivePass(one); !strings.Contains(got, "only message") {
		t.Errorf("ExtractivePass(one message) = %q, want it to contain the message", got)
	}
}

func TestExtractivePass_TwoMessagesAreBothKeptWithNoOmissionNote(t *testing.T) {
	first := provider.TextMessage("user", "first message")
	second := provider.TextMessage("assistant", "second message")
	got := ExtractivePass([]provider.Message{first, second})
	if want := line(first) + "\n" + line(second); got != want {
		t.Errorf("ExtractivePass(two messages) = %q, want %q", got, want)
	}
	if strings.Contains(got, "omitted") {
		t.Errorf("ExtractivePass(two messages) = %q, want no omission note when nothing was omitted", got)
	}
}

func TestCondensePrompt_InstructsAgainstClaimingAnEffectOutcome(t *testing.T) {
	// This is an instruction TO the condenser model, so it necessarily
	// names "completed"/"failed" — what matters is that it forbids
	// claiming either, matching internal/eventlog.Condensation's own
	// invariant (condensation_test.go) that a condensation can never
	// answer whether an external effect completed.
	transcript := []provider.Message{provider.TextMessage("tool", "the file was written")}
	prompt := CondensePrompt(transcript)
	lower := strings.ToLower(prompt.System)
	if !strings.Contains(lower, "never") || !strings.Contains(lower, "completed") {
		t.Errorf("CondensePrompt.System = %q, want an explicit instruction against claiming an effect completed", prompt.System)
	}
	if len(prompt.Messages) != 1 || prompt.Messages[0].Role != "user" {
		t.Errorf("CondensePrompt.Messages = %+v, want exactly one user message", prompt.Messages)
	}
}

// A "tool" message answers the tool_use blocks of the assistant message before
// it. A cut that leaves one as the first retained message summarises the calls
// away and keeps their results, which a hosted API refuses.
func TestShouldCondense_NeverStartsTheRetainedTailOnAToolMessage(t *testing.T) {
	call := func(id string) provider.Message {
		return provider.Message{Role: "assistant", Blocks: []provider.ContentBlock{{Kind: provider.BlockToolUse, ToolUseID: id, ToolName: "t", Input: []byte(`{}`)}}}
	}
	big := strings.Repeat("x", 500)
	transcript := []provider.Message{
		provider.TextMessage("user", "go"),
		call("a"), provider.ToolResultMessage("a", big, false),
		call("b"), provider.ToolResultMessage("b", big, false),
		call("c"), provider.ToolResultMessage("c", big, false),
		provider.TextMessage("assistant", "so far so good"),
		provider.TextMessage("user", "carry on"),
	}
	// len 9, so the plain cut is at 5 (an assistant call): fine as it stands.
	if ok, covered := ShouldCondense(transcript, 100); !ok || covered != 5 || transcript[covered].Role == "tool" {
		t.Fatalf("ShouldCondense = (%v, %d)", ok, covered)
	}

	// One message fewer puts the plain cut on the tool message that answers "c".
	shorter := transcript[:8]
	if shorter[len(shorter)-condenseKeepTail].Role != "tool" {
		t.Fatal("fixture: the plain cut should land on a tool message")
	}
	ok, covered := ShouldCondense(shorter, 100)
	if !ok || covered != 3 {
		t.Fatalf("ShouldCondense = (%v, %d), want the cut moved back to 3, before the assistant call that tool message answers", ok, covered)
	}
	if role := shorter[covered].Role; role != "assistant" {
		t.Fatalf("the retained tail starts with a %q message, want the call itself", role)
	}
	if err := provider.ValidateTranscript(shorter[covered:]); err != nil {
		t.Fatalf("the retained tail is not a valid transcript on its own: %v", err)
	}

	// With nothing before the pair to cover, there is nothing to condense.
	if ok, covered := ShouldCondense([]provider.Message{call("a"), provider.ToolResultMessage("a", big, false), call("b"), provider.ToolResultMessage("b", big, false), provider.TextMessage("assistant", "x")}, 100); ok {
		t.Fatalf("ShouldCondense = (true, %d), want false: the only cut that keeps pairs whole covers nothing", covered)
	}
}

// The summariser was shown a 256-byte stub of each message and nothing at all of a
// tool_use. It has to see what was said, what was asked of tools and what came back.
func TestCondensePrompt_ShowsTheRealMessages(t *testing.T) {
	transcript := []provider.Message{
		provider.TextMessage("user", "find the bug"),
		{Role: "assistant", Blocks: []provider.ContentBlock{
			{Kind: provider.BlockThinking, Text: "private reasoning"},
			{Kind: provider.BlockText, Text: "looking"},
			{Kind: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "platform/grep@v1", Input: []byte(`{"pattern":"TODO"}`)},
		}},
		provider.ToolResultMessage("tu1", strings.Repeat("hit\n", 500), false),
		provider.ToolResultMessage("tu2", "no such file", true),
	}
	got := CondensePrompt(transcript).Messages[0].PlainText()

	for _, want := range []string{
		"user: find the bug",
		"[called platform/grep@v1 with {\"pattern\":\"TODO\"}]",
		"looking",
		"[error result] no such file",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the summariser's input lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "private reasoning") {
		t.Error("reasoning was shown to the summariser")
	}
	// 2,000 bytes of result is well past the old 256-byte stub and under 4 KiB.
	if n := strings.Count(got, "hit"); n != 500 {
		t.Errorf("the tool result was cut to %d of 500 lines", n)
	}

	huge := CondensePrompt([]provider.Message{provider.ToolResultMessage("tu1", strings.Repeat("é", 10000), false)}).Messages[0].PlainText()
	if len(huge) > summaryTextBytes+64 || !utf8.ValidString(huge) {
		t.Errorf("a long result is %d bytes (valid UTF-8: %v), want it cut near %d on a character boundary", len(huge), utf8.ValidString(huge), summaryTextBytes)
	}
	long := CondensePrompt([]provider.Message{{Role: "assistant", Blocks: []provider.ContentBlock{
		{Kind: provider.BlockToolUse, ToolUseID: "x", ToolName: "t", Input: []byte(`"` + strings.Repeat("a", 2000) + `"`)},
	}}}).Messages[0].PlainText()
	if len(long) > summaryInputBytes+64 {
		t.Errorf("a long tool input is %d bytes, want it cut near %d", len(long), summaryInputBytes)
	}
}
