package provider_test

import (
	"errors"
	"testing"

	"github.com/truongpx396/nexus-agent/internal/provider"
)

func use(id string) provider.Message {
	return provider.Message{Role: "assistant", Blocks: []provider.ContentBlock{{Kind: provider.BlockToolUse, ToolUseID: id, ToolName: "t"}}}
}

func uses(ids ...string) provider.Message {
	m := provider.Message{Role: "assistant"}
	for _, id := range ids {
		m.Blocks = append(m.Blocks, provider.ContentBlock{Kind: provider.BlockToolUse, ToolUseID: id, ToolName: "t"})
	}
	return m
}

func result(ids ...string) provider.Message {
	m := provider.Message{Role: "tool"}
	for _, id := range ids {
		m.Blocks = append(m.Blocks, provider.ContentBlock{Kind: provider.BlockToolResult, ToolUseID: id, Text: "r"})
	}
	return m
}

func TestValidateTranscript(t *testing.T) {
	user := provider.TextMessage("user", "hi")
	cases := []struct {
		name string
		in   []provider.Message
		ok   bool
	}{
		{"empty", nil, true},
		{"plain conversation", []provider.Message{user, provider.TextMessage("assistant", "a"), user}, true},
		{"one call answered", []provider.Message{user, use("a"), result("a")}, true},
		{"parallel calls answered in one message", []provider.Message{user, uses("a", "b"), result("a", "b")}, true},
		{"parallel calls answered in two consecutive tool messages", []provider.Message{user, uses("a", "b"), result("a"), result("b")}, true},
		{"a call, its answer, then another call", []provider.Message{user, use("a"), result("a"), use("b"), result("b")}, true},
		{"a call with no answer at the end", []provider.Message{user, use("a")}, false},
		{"a call unanswered before the next assistant turn", []provider.Message{user, use("a"), provider.TextMessage("assistant", "x")}, false},
		{"a user turn between a call and its answer", []provider.Message{user, use("a"), user, result("a")}, false},
		{"a second answer for one call", []provider.Message{user, use("a"), result("a"), result("a")}, false},
		{"two answers in one message", []provider.Message{user, use("a"), result("a", "a")}, false},
		{"an answer with no call", []provider.Message{user, result("ghost")}, false},
		{"an answer that comes before its call", []provider.Message{user, result("a"), use("a")}, false},
		{"a repeated call id", []provider.Message{user, use("a"), result("a"), use("a"), result("a")}, false},
		{"one of two parallel calls unanswered", []provider.Message{user, uses("a", "b"), result("a")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := provider.ValidateTranscript(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("got %v, want a valid transcript", err)
			}
			if !tc.ok && !errors.Is(err, provider.ErrMalformedTranscript) {
				t.Fatalf("got %v, want ErrMalformedTranscript", err)
			}
		})
	}
}
