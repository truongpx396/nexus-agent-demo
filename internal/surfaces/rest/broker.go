package rest

import (
	"sync"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// published is one item flowing through the broker: a durably-appended
// event, or the error kernel.Kernel.Run yielded outside its own
// terminal-event paths (a marshal/seal/append failure, not a modeled
// TerminalReason).
type published struct {
	Event store.Event
	Err   error
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
