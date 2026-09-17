package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// plaintextDecrypt is a no-crypto DecryptFunc stand-in: Rehydrate/
// RehydrateTaint only ever call decrypt and unmarshal its result, so a
// test can hand them plaintext directly through Event.Payload without any
// real internal/crypto machinery.
func plaintextDecrypt(_ context.Context, e store.Event) ([]byte, error) {
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
	history := []store.Event{
		{Type: store.EventUserMessage, Payload: []byte(`{}`)},
		{Type: store.EventContent, Payload: []byte(`{}`)},
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
	history := []store.Event{
		{Type: store.EventUserMessage, Payload: []byte(`{}`)},
		{Type: store.EventTaintTransition, Payload: marshalTaint(t, [3]bool{true, false, false})},
		{Type: store.EventToolResult, Payload: []byte(`{}`)},
		{Type: store.EventTaintTransition, Payload: marshalTaint(t, [3]bool{true, true, false})},
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
	history := []store.Event{{Type: store.EventTaintTransition, Payload: foreign}}
	got, err := RehydrateTaint(context.Background(), history, plaintextDecrypt)
	if err != nil {
		t.Fatalf("RehydrateTaint: %v", err)
	}
	if got != ([3]bool{false, true, true}) {
		t.Fatalf("got %v, want [false true true] read from the foreign payload's own engaged field", got)
	}
}

func TestRehydrateTaint_DecryptErrorPropagates(t *testing.T) {
	history := []store.Event{{Type: store.EventTaintTransition, Payload: []byte(`{}`)}}
	wantErr := errors.New("boom")
	_, err := RehydrateTaint(context.Background(), history, func(context.Context, store.Event) ([]byte, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, wantErr)
	}
}
