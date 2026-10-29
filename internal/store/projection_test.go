package store

import (
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

func TestReplayProjection(t *testing.T) {
	tests := []struct {
		name   string
		events []eventlog.EventType
		want   string
	}{
		{"empty history", nil, eventlog.SessionStatusQueued},
		{"a user message alone is running", []eventlog.EventType{eventlog.EventUserMessage}, eventlog.SessionStatusRunning},
		{"content then terminal is ended", []eventlog.EventType{eventlog.EventUserMessage, eventlog.EventContent, eventlog.EventTerminal}, eventlog.SessionStatusCompleted},
		{"an unresolved approval_requested is suspended", []eventlog.EventType{eventlog.EventUserMessage, eventlog.EventToolUse, eventlog.EventApprovalRequested}, eventlog.SessionStatusSuspended},
		{"a resolved approval returns to running", []eventlog.EventType{eventlog.EventApprovalRequested, eventlog.EventApprovalGranted, eventlog.EventToolResult}, eventlog.SessionStatusRunning},
		{"an unresolved input_requested is suspended", []eventlog.EventType{eventlog.EventUserMessage, eventlog.EventInputRequested}, eventlog.SessionStatusSuspended},
		{"a resolved input_requested returns to running", []eventlog.EventType{eventlog.EventInputRequested, eventlog.EventInputAnswered}, eventlog.SessionStatusRunning},
		{"budget_decision and tool_loaded never change status on their own", []eventlog.EventType{eventlog.EventToolLoaded, eventlog.EventBudgetDecision}, eventlog.SessionStatusQueued},
		{"a conversational pause is awaiting_input, not terminal", []eventlog.EventType{eventlog.EventUserMessage, eventlog.EventContent, eventlog.EventAwaitingInput}, eventlog.SessionStatusAwaitingInput},
		{"a second turn after a conversational pause returns to running", []eventlog.EventType{eventlog.EventAwaitingInput, eventlog.EventUserMessage}, eventlog.SessionStatusRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var history []eventlog.Event
			for _, typ := range tt.events {
				history = append(history, eventlog.Event{Type: typ})
			}
			got := ReplayProjection(history)
			if got.Status != tt.want {
				t.Fatalf("ReplayProjection(%v).Status = %q, want %q", tt.events, got.Status, tt.want)
			}
		})
	}
}

// TestReplayFullProjection_DistinguishesCompletedFromFailed is the
// selective-decrypt layer's own reason for existing: pure structural replay
// alone cannot tell a clean completion from a failure (both are just "an
// EventTerminal happened"), only ReplayFullProjection's one-event decrypt
// can.
func TestReplayFullProjection_DistinguishesCompletedFromFailed(t *testing.T) {
	decryptReason := func(reason string) TerminalDecryptFunc {
		return func(eventlog.Event) ([]byte, error) {
			return []byte(`{"reason":"` + reason + `"}`), nil
		}
	}

	completed := []eventlog.Event{{Type: eventlog.EventUserMessage}, {Type: eventlog.EventTerminal}}
	p, err := ReplayFullProjection(completed, decryptReason("completed"))
	if err != nil {
		t.Fatalf("ReplayFullProjection: %v", err)
	}
	if p.Status != eventlog.SessionStatusCompleted || p.TerminalReason == nil || *p.TerminalReason != "completed" {
		t.Fatalf("got %+v, want status=completed reason=completed", p)
	}

	failed := []eventlog.Event{{Type: eventlog.EventUserMessage}, {Type: eventlog.EventTerminal}}
	p, err = ReplayFullProjection(failed, decryptReason("cost_exhausted"))
	if err != nil {
		t.Fatalf("ReplayFullProjection: %v", err)
	}
	if p.Status != eventlog.SessionStatusFailed || p.TerminalReason == nil || *p.TerminalReason != "cost_exhausted" {
		t.Fatalf("got %+v, want status=failed reason=cost_exhausted", p)
	}
}
