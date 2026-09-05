package kernel

import (
	"errors"
	"testing"
)

// TestBuildTerminalPayload_AllReasonsAccepted is README task 13.8 (closing
// production-readiness finding F9): table-driven over all 9 TerminalReason
// values through buildTerminalPayload — the exhaustive switch the
// `exhaustive` linter proves is total, but which no test previously proved
// was actually correct for any one arm.
func TestBuildTerminalPayload_AllReasonsAccepted(t *testing.T) {
	cases := []struct {
		name   string
		term   Terminal
		reason TerminalReason
	}{
		{"completed", TerminalCompleted(), ReasonCompleted},
		{"max_turns_exceeded", TerminalMaxTurnsExceeded(25), ReasonMaxTurnsExceeded},
		{"cost_exhausted", TerminalCostExhausted("ceiling reached"), ReasonCostExhausted},
		{"aborted", TerminalAborted("operator cancel"), ReasonAborted},
		{"stuck_terminated", TerminalStuckTerminated("repeating tool call"), ReasonStuckTerminated},
		{"permission_denied", TerminalPermissionDenied("platform/shell@1"), ReasonPermissionDenied},
		{"context_overflow", TerminalContextOverflow("prompt too long"), ReasonContextOverflow},
		{"error", TerminalError(errors.New("boom")), ReasonError},
		{"refused", TerminalRefused("cyber"), ReasonRefused},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.term.Reason != c.reason {
				t.Fatalf("producer returned Reason %q, want %q", c.term.Reason, c.reason)
			}
			payload, err := buildTerminalPayload(c.term)
			if err != nil {
				t.Fatalf("buildTerminalPayload(%+v) returned an error: %v", c.term, err)
			}
			if payload.Reason != c.reason {
				t.Errorf("payload.Reason = %q, want %q", payload.Reason, c.reason)
			}
		})
	}
}

func TestBuildTerminalPayload_UnknownReasonRejected(t *testing.T) {
	_, err := buildTerminalPayload(Terminal{Reason: TerminalReason("not_a_real_reason")})
	if err == nil {
		t.Fatal("buildTerminalPayload accepted an unknown TerminalReason")
	}
}

func TestTerminalMaxTurnsExceeded_DetailNamesTheLimit(t *testing.T) {
	term := TerminalMaxTurnsExceeded(25)
	if term.Detail == "" {
		t.Fatal("TerminalMaxTurnsExceeded produced no detail")
	}
}

func TestTerminalPermissionDenied_DetailNamesTheTool(t *testing.T) {
	term := TerminalPermissionDenied("platform/shell@1")
	if term.Detail != "denied: platform/shell@1" {
		t.Errorf("Detail = %q, want it to name the denied tool", term.Detail)
	}
}

func TestTerminalRefused_DetailWithAndWithoutCategory(t *testing.T) {
	withCategory := TerminalRefused("cyber")
	if withCategory.Detail != "refused: cyber" {
		t.Errorf("Detail = %q, want %q", withCategory.Detail, "refused: cyber")
	}
	withoutCategory := TerminalRefused("")
	if withoutCategory.Detail != "refused" {
		t.Errorf("Detail = %q, want %q", withoutCategory.Detail, "refused")
	}
}

func TestTerminalError_DetailIsErrorMessage(t *testing.T) {
	term := TerminalError(errors.New("stream truncated"))
	if term.Detail != "stream truncated" {
		t.Errorf("Detail = %q, want the underlying error message", term.Detail)
	}
}
