package rest

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

const (
	eventsChannelPrefix = "nexus:events:"
	deltasChannelPrefix = "nexus:deltas:"
)

// EventsChannel/DeltasChannel are this process's own Redis Pub/Sub
// channel-naming convention — exported because cmd/nexusd has two separate
// reasons to know it: startEventBus PSubscribes to the wildcard form of
// both ("nexus:events:*" / "nexus:deltas:*") for this package's own
// broker, and telegram/zalo/email's surface adapters (which have no
// broker of their own) SUBSCRIBE to one session's EventsChannel directly
// so their own RunStarter/Resumer channel contract — "carries every event
// of the run" — still holds even though the run itself now executes on a
// queue worker, not a local goroutine.
func EventsChannel(sessionID uuid.UUID) string { return eventsChannelPrefix + sessionID.String() }
func DeltasChannel(sessionID uuid.UUID) string { return deltasChannelPrefix + sessionID.String() }

// ParseBusChannel is EventsChannel/DeltasChannel's own inverse —
// cmd/nexusd's startEventBus is the only caller, kept in this package so
// the channel-naming convention lives in exactly one place rather than
// being duplicated across the publish and subscribe sides.
func ParseBusChannel(channel string) (sessionID uuid.UUID, isDelta bool, ok bool) {
	var idStr string
	switch {
	case strings.HasPrefix(channel, eventsChannelPrefix):
		idStr = strings.TrimPrefix(channel, eventsChannelPrefix)
	case strings.HasPrefix(channel, deltasChannelPrefix):
		idStr, isDelta = strings.TrimPrefix(channel, deltasChannelPrefix), true
	default:
		return uuid.UUID{}, false, false
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return uuid.UUID{}, false, false
	}
	return id, isDelta, true
}

// EventBusMessage is one durable event's wire shape over the bus —
// store.Event plus the one piece of Go state a channel-of-(Event,error)
// pair also carries that a plain struct can't (published's own Err field),
// flattened to a string since an error value itself doesn't marshal.
// Exported so cmd/nexusd's startEventBus can decode it without this
// package needing to expose its own demux loop.
type EventBusMessage struct {
	Event *store.Event `json:"event,omitempty"`
	Err   string       `json:"err,omitempty"`
}

// DeltaDTO is one live, best-effort preview chunk — kernel.Kernel.OnChunk's
// ChunkEvent, translated for the wire. It is never durable and never
// replayed from Postgres history the way an ordinary store.Event is
// (handleEvents' history replay only ever reads store.Event rows) — a
// subscriber that connects a moment too late simply never sees the ones it
// missed. That's fine: the durable EventContent/EventToolUse this preview is
// standing in for still lands right behind it, over the same channel, and
// IS replayable. Same shape Anthropic's own content_block_delta or
// LangChain's on_chat_model_stream chunks have relative to the final
// assembled message/checkpoint: ephemeral by design, not a weaker copy of
// the durable record.
type DeltaDTO struct {
	Kind      string `json:"kind"`                  // "content" | "tool_use" | "reasoning"
	Text      string `json:"text,omitempty"`        // Kind == "content" — never set for "reasoning" (see kernel.Kernel.OnChunk's own doc comment)
	ToolUseID string `json:"tool_use_id,omitempty"` // Kind == "tool_use"
	ToolName  string `json:"tool_name,omitempty"`   // Kind == "tool_use"
}

// published is one item flowing through the broker: a durably-appended
// event, the error kernel.Kernel.Run yielded outside its own terminal-event
// paths (a marshal/seal/append failure, not a modeled TerminalReason), or a
// live Delta — mutually exclusive with Event/Err, and never carries a Seq,
// which is exactly what lets handleEvents skip its Seq-based
// already-replayed check for one of these and forward it unconditionally.
type published struct {
	Event store.Event
	Err   error
	Delta *DeltaDTO
}

// broker is an in-memory per-session pub/sub the run's event-draining
// goroutine (server.go's publishUntilDone) publishes to as the run produces
// events — a delivery optimization, not a second source of truth:
// store.Append is always the durable write, and always happens first
// (kernel/loop.go). A subscriber that connects after a run has already
// finished sees nothing here; handleEvents falls back to replaying the log
// from Postgres for that case, and combines the two (subscribe before
// replay, then discard anything the live channel redelivers that the replay
// already sent) to make the two sources gapless and duplicate-free together
// — this type alone only guarantees "no misses for anyone already
// subscribed"; it is deliberately not gapless on its own; a subscriber slow
// enough to fill its buffered channel (publish's non-blocking send) silently
// drops further live events until it reconnects and replays. Real
// at-least-once outbox delivery is Phase 7's (README task 7.10).
type broker struct {
	mu   sync.Mutex
	subs map[uuid.UUID][]chan published
}

func newBroker() *broker {
	return &broker{subs: map[uuid.UUID][]chan published{}}
}

// subscribe registers a new channel for sessionID. unsubscribe only removes
// it from the registry — it never closes the channel itself, so a
// subscribe/unsubscribe race with closeSession can never double-close (only
// closeSession ever closes a channel, and only once, under the same mutex
// that removes it from the map).
func (b *broker) subscribe(sessionID uuid.UUID) (ch chan published, unsubscribe func()) {
	ch = make(chan published, 64)
	b.mu.Lock()
	b.subs[sessionID] = append(b.subs[sessionID], ch)
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		subs := b.subs[sessionID]
		for i, c := range subs {
			if c == ch {
				b.subs[sessionID] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
	}
}

// publish fans p out to every current subscriber of sessionID. A full
// subscriber channel is skipped rather than blocked on — a slow SSE client
// must never stall the run itself; it falls back to the Postgres replay
// path on its next read.
func (b *broker) publish(sessionID uuid.UUID, p published) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs[sessionID] {
		select {
		case ch <- p:
		default:
		}
	}
}

// PublishDelta fans a live preview chunk out to sessionID's current
// subscribers, the same non-blocking, no-subscriber-is-fine delivery
// publish already gives an ordinary durable event. cmd/nexusd is the only
// caller — kernel.Kernel.OnChunk is wired to this method in serve.go,
// mirroring how Receipts/OnSuspend/OnDelegate are each wired to a method on
// some other package from that same file. A session nobody is currently
// subscribed to (the common case for a queue-resumed run with no attached
// client) makes this a no-op, not an error.
//
// With s.Bus set (cmd/nexusd always sets it), this publishes to Redis
// instead of calling the local broker directly — startEventBus's own
// subscriber loop is what actually calls RelayDelta, so a same-process
// caller and a cross-process worker go through the exact same path. Unlike
// a durable store.Event's own Payload (already sealed ciphertext), d.Text
// is live, unencrypted preview content (DeltaDTO's own doc comment: "never
// durable, never replayed") — publishing it here means Redis, and the
// network path to it, carries plaintext for the first time (today it only
// ever sees lock tokens, cost counters, and OAuth CSRF state). Acceptable
// only because Redis is already assumed private-network/trusted, the same
// assumption this deployment's session lock and cost gate already lean on.
func (s *Server) PublishDelta(sessionID uuid.UUID, d DeltaDTO) {
	if s.Bus == nil {
		s.broker.publish(sessionID, published{Delta: &d})
		return
	}
	raw, err := json.Marshal(d)
	if err != nil {
		log.Error().Err(err).Any("session_id", sessionID).Msg("rest: marshal delta for bus")
		return
	}
	if err := s.Bus.Publish(context.Background(), DeltasChannel(sessionID), raw); err != nil {
		log.Error().Err(err).Any("session_id", sessionID).Msg("rest: publish delta to bus")
	}
}

// PublishEvent is every consumer of one of a run's own durably-appended
// events converging on a single call: internal/queue's worker pool
// (cmd/nexusd's queueRunner.Run, draining internal/runctl.Control.Resume
// for a queued job) and this package's own publishUntilDone (a synchronous
// Kernel.Seed's handful of events) both call this for every event, so
// approval-outbox delivery, terminal-span emission, and the SSE fan-out all
// happen exactly once no matter which path — or which process — actually
// produced the event. store.Append is always the durable write and always
// happens first (kernel/loop.go); everything this method does is a
// delivery/side-effect optimization on top of that, never a second source
// of truth.
func (s *Server) PublishEvent(tenantID, sessionID uuid.UUID, e store.Event, evErr error) {
	if s.Bus == nil {
		s.RelayEvent(sessionID, e, evErr)
	} else if raw, err := json.Marshal(EventBusMessage{Event: &e, Err: errString(evErr)}); err != nil {
		log.Error().Err(err).Any("session_id", sessionID).Msg("rest: marshal event for bus")
	} else if err := s.Bus.Publish(context.Background(), EventsChannel(sessionID), raw); err != nil {
		log.Error().Err(err).Any("session_id", sessionID).Msg("rest: publish event to bus")
	}

	if evErr != nil {
		return
	}
	if s.Outbox != nil && s.OutboxSender != nil && e.Type == store.EventApprovalRequested {
		s.deliverApprovalNotification(sessionID, e)
	}
	if s.Exporter != nil && e.Type == store.EventTerminal {
		s.emitTerminalSpan(tenantID, sessionID)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// RelayEvent and RelayDelta are the ONLY callers of broker.publish/
// closeSession left in this package once Bus is set — startEventBus's own
// subscriber loop (cmd/nexusd) calls these after decoding a message off
// Redis Pub/Sub, so a same-process fast path (Bus nil, see publishEvent/
// PublishDelta above) and a genuinely cross-process worker's publish both
// end up here, through one mechanism, not two. RelayEvent additionally
// closes the session's local subscribers the moment a TERMINAL event comes
// through — publishUntilDone no longer does this itself (a queued run's own
// returned channel now closes as soon as its synchronous Kernel.Seed step
// finishes, long before the run's REAL terminal event exists), so closing
// on terminal-event-observed, not on channel-closed, is what keeps an SSE
// client waiting correctly across however long the queued turn loop
// actually takes, on whichever process runs it.
func (s *Server) RelayEvent(sessionID uuid.UUID, e store.Event, evErr error) {
	s.broker.publish(sessionID, published{Event: e, Err: evErr})
	if evErr == nil && e.Type == store.EventTerminal {
		s.broker.closeSession(sessionID)
	}
}

func (s *Server) RelayDelta(sessionID uuid.UUID, d DeltaDTO) {
	s.broker.publish(sessionID, published{Delta: &d})
}

// closeSession closes every remaining subscriber channel for sessionID and
// forgets them — called once, when the run's generator (kernel.Kernel.Run)
// has finished, so any SSE handler still reading unblocks even on an
// abnormal end that produced no terminal event.
func (b *broker) closeSession(sessionID uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs[sessionID] {
		close(ch)
	}
	delete(b.subs, sessionID)
}
