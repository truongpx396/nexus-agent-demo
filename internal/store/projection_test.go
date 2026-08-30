package store

import "testing"

func TestReplayProjection(t *testing.T) {
	tests := []struct {
		name   string
		events []EventType
		want   string
	}{
		{"empty history", nil, SessionStatusQueued},
		{"a user message alone is running", []EventType{EventUserMessage}, SessionStatusRunning},
		{"content then terminal is ended", []EventType{EventUserMessage, EventContent, EventTerminal}, SessionStatusCompleted},
		{"an unresolved approval_requested is suspended", []EventType{EventUserMessage, EventToolUse, EventApprovalRequested}, SessionStatusSuspended},
		{"a resolved approval returns to running", []EventType{EventApprovalRequested, EventApprovalGranted, EventToolResult}, SessionStatusRunning},
		{"an unresolved input_requested is suspended", []EventType{EventUserMessage, EventInputRequested}, SessionStatusSuspended},
		{"a resolved input_requested returns to running", []EventType{EventInputRequested, EventInputAnswered}, SessionStatusRunning},
		{"budget_decision and tool_loaded never change status on their own", []EventType{EventToolLoaded, EventBudgetDecision}, SessionStatusQueued},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var history []Event
			for _, typ := range tt.events {
				history = append(history, Event{Type: typ})
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
		return func(Event) ([]byte, error) {
			return []byte(`{"reason":"` + reason + `"}`), nil
		}
	}

	completed := []Event{{Type: EventUserMessage}, {Type: EventTerminal}}
	p, err := ReplayFullProjection(completed, decryptReason("completed"))
	if err != nil {
		t.Fatalf("ReplayFullProjection: %v", err)
	}
	if p.Status != SessionStatusCompleted || p.TerminalReason == nil || *p.TerminalReason != "completed" {
		t.Fatalf("got %+v, want status=completed reason=completed", p)
	}

	failed := []Event{{Type: EventUserMessage}, {Type: EventTerminal}}
	p, err = ReplayFullProjection(failed, decryptReason("cost_exhausted"))
	if err != nil {
		t.Fatalf("ReplayFullProjection: %v", err)
	}
	if p.Status != SessionStatusFailed || p.TerminalReason == nil || *p.TerminalReason != "cost_exhausted" {
		t.Fatalf("got %+v, want status=failed reason=cost_exhausted", p)
	}
}
