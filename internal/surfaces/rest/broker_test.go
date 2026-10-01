package rest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// fakeBus is a minimal in-memory EventBus for testing PublishEvent/
// PublishDelta's own Bus-set branch without a real Redis — Redis Pub/Sub's
// own wire behavior is a well-trusted library operation; what this
// package's own code needs proving is that it builds the right channel
// name and the right wire payload, and that RelayEvent/RelayDelta (the
// other end of that same contract) correctly feed the local broker back.
type fakeBus struct {
	published []busCall
}

type busCall struct {
	channel string
	payload []byte
}

func (b *fakeBus) Publish(_ context.Context, channel string, payload []byte) error {
	b.published = append(b.published, busCall{channel: channel, payload: payload})
	return nil
}

func TestParseBusChannel_RoundTripsEventsAndDeltas(t *testing.T) {
	sessionID := uuid.New()

	gotID, isDelta, ok := ParseBusChannel(EventsChannel(sessionID))
	if !ok || isDelta || gotID != sessionID {
		t.Fatalf("ParseBusChannel(EventsChannel(id)) = (%s, delta=%v, ok=%v), want (%s, false, true)", gotID, isDelta, ok, sessionID)
	}

	gotID, isDelta, ok = ParseBusChannel(DeltasChannel(sessionID))
	if !ok || !isDelta || gotID != sessionID {
		t.Fatalf("ParseBusChannel(DeltasChannel(id)) = (%s, delta=%v, ok=%v), want (%s, true, true)", gotID, isDelta, ok, sessionID)
	}

	if _, _, ok := ParseBusChannel("nexus:something-else:" + sessionID.String()); ok {
		t.Fatal("ParseBusChannel accepted an unrecognized channel prefix")
	}
	if _, _, ok := ParseBusChannel("nexus:events:not-a-uuid"); ok {
		t.Fatal("ParseBusChannel accepted a non-UUID session id")
	}
}

func TestPublishEvent_WithBusSet_PublishesTheWireShapeRelayEventExpects(t *testing.T) {
	bus := &fakeBus{}
	s := &Server{broker: newBroker(), Bus: bus}
	sessionID := uuid.New()
	tenantID := uuid.New()
	ev := store.Event{EventID: uuid.New(), SessionID: sessionID, TenantID: tenantID, Type: store.EventContent, Seq: 3}

	s.PublishEvent(tenantID, sessionID, ev, nil)

	if len(bus.published) != 1 {
		t.Fatalf("Bus.Publish called %d times, want 1", len(bus.published))
	}
	call := bus.published[0]
	if want := EventsChannel(sessionID); call.channel != want {
		t.Fatalf("published channel = %q, want %q", call.channel, want)
	}
	var wire EventBusMessage
	if err := json.Unmarshal(call.payload, &wire); err != nil {
		t.Fatalf("unmarshal published payload: %v", err)
	}
	if wire.Err != "" {
		t.Fatalf("wire.Err = %q, want empty (no error was published)", wire.Err)
	}
	if wire.Event == nil || wire.Event.EventID != ev.EventID {
		t.Fatalf("wire.Event = %+v, want event id %s", wire.Event, ev.EventID)
	}
}

func TestPublishEvent_WithBusSet_CarriesTheErrorAsAString(t *testing.T) {
	bus := &fakeBus{}
	s := &Server{broker: newBroker(), Bus: bus}
	sessionID := uuid.New()
	runErr := errors.New("seal event payload: boom")

	s.PublishEvent(uuid.New(), sessionID, store.Event{SessionID: sessionID}, runErr)

	if len(bus.published) != 1 {
		t.Fatalf("Bus.Publish called %d times, want 1", len(bus.published))
	}
	var wire EventBusMessage
	if err := json.Unmarshal(bus.published[0].payload, &wire); err != nil {
		t.Fatalf("unmarshal published payload: %v", err)
	}
	if wire.Err != runErr.Error() {
		t.Fatalf("wire.Err = %q, want %q", wire.Err, runErr.Error())
	}
}

func TestPublishDelta_WithBusSet_PublishesToTheDeltasChannel(t *testing.T) {
	bus := &fakeBus{}
	s := &Server{broker: newBroker(), Bus: bus}
	sessionID := uuid.New()
	d := DeltaDTO{Kind: "content", Text: "hello"}

	s.PublishDelta(sessionID, d)

	if len(bus.published) != 1 {
		t.Fatalf("Bus.Publish called %d times, want 1", len(bus.published))
	}
	call := bus.published[0]
	if want := DeltasChannel(sessionID); call.channel != want {
		t.Fatalf("published channel = %q, want %q", call.channel, want)
	}
	var got DeltaDTO
	if err := json.Unmarshal(call.payload, &got); err != nil {
		t.Fatalf("unmarshal published payload: %v", err)
	}
	if got != d {
		t.Fatalf("published delta = %+v, want %+v", got, d)
	}
}

// TestRelayEvent_ClosesLocalSubscribersOnTerminal proves RelayEvent's own
// side of the contract PublishEvent's wire shape exists for: this is what
// cmd/nexusd's startEventBus calls after decoding a message a DIFFERENT
// process (a queue worker) published — a same-process publish (Bus nil)
// and a genuinely cross-process one both have to end up here, and a
// subscriber must see the terminal event AND have its channel closed
// right after, exactly like the old same-process broker.publish +
// closeSession pairing did.
func TestRelayEvent_ClosesLocalSubscribersOnTerminal(t *testing.T) {
	s := &Server{broker: newBroker()}
	sessionID := uuid.New()
	ch, unsubscribe := s.broker.subscribe(sessionID)
	defer unsubscribe()

	s.RelayEvent(sessionID, store.Event{SessionID: sessionID, Type: store.EventContent}, nil)
	got, ok := <-ch
	if !ok {
		t.Fatal("channel closed after a non-terminal event, want still open")
	}
	if got.Event.Type != store.EventContent {
		t.Fatalf("relayed event type = %v, want content", got.Event.Type)
	}

	s.RelayEvent(sessionID, store.Event{SessionID: sessionID, Type: store.EventTerminal}, nil)
	got, ok = <-ch
	if !ok {
		t.Fatal("channel closed before delivering the terminal event itself")
	}
	if got.Event.Type != store.EventTerminal {
		t.Fatalf("relayed event type = %v, want terminal", got.Event.Type)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel still open after a terminal event, want closed")
	}
}

func TestRelayDelta_FeedsTheLocalBroker(t *testing.T) {
	s := &Server{broker: newBroker()}
	sessionID := uuid.New()
	ch, unsubscribe := s.broker.subscribe(sessionID)
	defer unsubscribe()

	s.RelayDelta(sessionID, DeltaDTO{Kind: "content", Text: "hi"})

	got, ok := <-ch
	if !ok {
		t.Fatal("channel closed after a delta, want still open")
	}
	if got.Delta == nil || got.Delta.Text != "hi" {
		t.Fatalf("relayed delta = %+v, want Text=%q", got.Delta, "hi")
	}
}
