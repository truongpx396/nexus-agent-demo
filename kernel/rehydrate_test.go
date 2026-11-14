package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent/internal/eventlog"
	"github.com/truongpx396/nexus-agent/internal/provider"
)

// plaintextDecrypt is a no-crypto DecryptFunc stand-in: Rehydrate/
// RehydrateTaint only ever call decrypt and unmarshal its result, so a
// test can hand them plaintext directly through Event.Payload without any
// real internal/crypto machinery.
func plaintextDecrypt(_ context.Context, e eventlog.Event) ([]byte, error) {
	return e.Payload, nil
}

func marshalTaint(t *testing.T, engaged [3]bool) []byte {
	t.Helper()
	b, err := json.Marshal(taintTransitionPayload{Engaged: engaged})
	if err != nil {
		t.Fatalf("marshal taintTransitionPayload: %v", err)
	}
	return b
}

func TestRehydrateTaint_NoTransitionEverRecordedReturnsZeroValue(t *testing.T) {
	history := []eventlog.Event{
		{Type: eventlog.EventUserMessage, Payload: []byte(`{}`)},
		{Type: eventlog.EventContent, Payload: []byte(`{}`)},
	}
	got, err := RehydrateTaint(context.Background(), history, plaintextDecrypt)
	if err != nil {
		t.Fatalf("RehydrateTaint: %v", err)
	}
	if got != ([3]bool{}) {
		t.Fatalf("got %v, want the zero value (this session never engaged any leg)", got)
	}
}

func TestRehydrateTaint_ReturnsTheMostRecentTransition(t *testing.T) {
	history := []eventlog.Event{
		{Type: eventlog.EventUserMessage, Payload: []byte(`{}`)},
		{Type: eventlog.EventTaintTransition, Payload: marshalTaint(t, [3]bool{true, false, false})},
		{Type: eventlog.EventToolResult, Payload: []byte(`{}`)},
		{Type: eventlog.EventTaintTransition, Payload: marshalTaint(t, [3]bool{true, true, false})},
	}
	got, err := RehydrateTaint(context.Background(), history, plaintextDecrypt)
	if err != nil {
		t.Fatalf("RehydrateTaint: %v", err)
	}
	if got != ([3]bool{true, true, false}) {
		t.Fatalf("got %v, want the LAST transition's own cumulative state, not the first or a fold of both", got)
	}
}

// TestRehydrateTaint_ReadsAForeignProducersPayloadShape proves
// RehydrateTaint works against EventTaintTransition events written by
// internal/delegate or internal/teams too, not just kernel's own producer
// — those packages each carry their own extra fields alongside "engaged"
// (internal/teams/events.go's own doc comment: "each producer defines and
// reads back its own payload... nothing reads taint_transition payloads
// generically across packages") — RehydrateTaint only needs the shared
// field, which json.Unmarshal picks out regardless of what else is there.
func TestRehydrateTaint_ReadsAForeignProducersPayloadShape(t *testing.T) {
	foreign := []byte(`{"child_session_id":"11111111-1111-1111-1111-111111111111","engaged":[false,true,true]}`)
	history := []eventlog.Event{{Type: eventlog.EventTaintTransition, Payload: foreign}}
	got, err := RehydrateTaint(context.Background(), history, plaintextDecrypt)
	if err != nil {
		t.Fatalf("RehydrateTaint: %v", err)
	}
	if got != ([3]bool{false, true, true}) {
		t.Fatalf("got %v, want [false true true] read from the foreign payload's own engaged field", got)
	}
}

func TestRehydrateTaint_DecryptErrorPropagates(t *testing.T) {
	history := []eventlog.Event{{Type: eventlog.EventTaintTransition, Payload: []byte(`{}`)}}
	wantErr := errors.New("boom")
	_, err := RehydrateTaint(context.Background(), history, func(context.Context, eventlog.Event) ([]byte, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, wantErr)
	}
}

// answersFor rehydrates history and returns the text of every tool_result block
// the model would be shown for tool_use id "tu1".
func answersFor(t *testing.T, history []eventlog.Event) []string {
	t.Helper()
	transcript, _, err := Rehydrate(context.Background(), history, plaintextDecrypt)
	if err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	var out []string
	for _, m := range transcript {
		for _, b := range m.Blocks {
			if b.Kind == provider.BlockToolResult && b.ToolUseID == "tu1" {
				out = append(out, b.Text)
			}
		}
	}
	return out
}

func toolUseEvent(t *testing.T, id uuid.UUID) eventlog.Event {
	t.Helper()
	b, err := json.Marshal(toolUsePayload{ToolName: "platform/echo@v1", Input: json.RawMessage(`{}`), ToolUseID: "tu1"})
	if err != nil {
		t.Fatal(err)
	}
	return eventlog.Event{EventID: id, Type: eventlog.EventToolUse, Payload: b}
}

func toolResultEvent(t *testing.T, pair uuid.UUID, p toolResultPayload) eventlog.Event {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	ref := pair
	return eventlog.Event{EventID: uuid.New(), Type: eventlog.EventToolResult, PairRef: &ref, Payload: b}
}

var (
	approvalPlaceholder   = toolResultPayload{IsError: true, Reason: "approval required", AwaitingApproval: true}
	delegationPlaceholder = toolResultPayload{IsError: true, Reason: "delegating", AwaitingDelegation: true}
	realResult            = toolResultPayload{Output: json.RawMessage(`"real"`)}
)

func TestRehydrate_AnUnresolvedCallIsAnsweredByItsPlaceholder(t *testing.T) {
	use := uuid.New()
	got := answersFor(t, []eventlog.Event{toolUseEvent(t, use), toolResultEvent(t, use, approvalPlaceholder)})
	if len(got) != 1 || got[0] != "error: approval required" {
		t.Fatalf("answers = %q, want the placeholder alone while the call is unresolved", got)
	}
}

func TestRehydrate_ARealResultReplacesThePlaceholderInsteadOfJoiningIt(t *testing.T) {
	for name, placeholder := range map[string]toolResultPayload{"approval": approvalPlaceholder, "delegation": delegationPlaceholder} {
		t.Run(name, func(t *testing.T) {
			use := uuid.New()
			got := answersFor(t, []eventlog.Event{toolUseEvent(t, use), toolResultEvent(t, use, placeholder), toolResultEvent(t, use, realResult)})
			if len(got) != 1 || got[0] != `"real"` {
				t.Fatalf("answers = %q, want exactly the real result: two tool_results for one tool_use is a 400 from a hosted API", got)
			}
		})
	}
}

func TestRehydrate_TheFirstRealResultWinsAndAnOrphanIsDropped(t *testing.T) {
	use := uuid.New()
	second := toolResultPayload{Output: json.RawMessage(`"second"`)}
	got := answersFor(t, []eventlog.Event{
		toolUseEvent(t, use),
		toolResultEvent(t, use, realResult),
		toolResultEvent(t, use, second),
		toolResultEvent(t, uuid.New(), realResult), // pairs with no tool_use in this history
	})
	if len(got) != 1 || got[0] != `"real"` {
		t.Fatalf("answers = %q, want the first real result only", got)
	}
}
