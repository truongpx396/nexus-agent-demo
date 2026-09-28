package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
)

// redisEventBus adapts *redis.Client to rest.EventBus — a single Publish
// call is all Redis Pub/Sub needs here; the consumer-group/XADD machinery
// a durable job queue needs belongs to internal/queue, not this.
type redisEventBus struct{ client *redis.Client }

func (b redisEventBus) Publish(ctx context.Context, channel string, payload []byte) error {
	return b.client.Publish(ctx, channel, payload).Err()
}

// startEventBus is this process's one static Redis Pub/Sub subscription,
// covering every session at once (rest.PublishDelta/PublishEvent's own
// "nexus:events:*"/"nexus:deltas:*" channel-naming convention) rather than
// a per-session dynamic subscribe/unsubscribe mirroring rest's own
// broker.subscribe/unsubscribe lifecycle: that would be the more
// scale-correct design (bounded to real local demand) but needs
// reference-counting bookkeeping this repo's actual scale (a demo, dozens
// of sessions, not millions) doesn't justify. One static
// pattern-subscription per process is O(1) regardless of session count and
// needs no lifecycle beyond process start/stop. If PSUBSCRIBE's
// O(all-processes x all-publishes) fan-out cost ever matters, per-session
// subscribe/unsubscribe is the thing to revisit first — named here the
// same way internal/queue.AdmissionController's own doc comment defers
// per-tenant fairness.
//
// This goroutine is the ONLY caller of rest.Server.RelayEvent/RelayDelta in
// the whole binary: a same-process publish (rest.Server.Bus nil) and a
// genuinely cross-process worker's publish both get republished to Redis
// and relayed back through here — one mechanism, not two.
func startEventBus(ctx context.Context, redisClient *redis.Client, srv *rest.Server) (stop func()) {
	sub := redisClient.PSubscribe(ctx, "nexus:events:*", "nexus:deltas:*")
	go func() {
		for msg := range sub.Channel() {
			sessionID, isDelta, ok := rest.ParseBusChannel(msg.Channel)
			if !ok {
				log.Warn().Str("channel", msg.Channel).Msg("nexusd: event bus: unrecognized channel")
				continue
			}
			if isDelta {
				var d rest.DeltaDTO
				if err := json.Unmarshal([]byte(msg.Payload), &d); err != nil {
					log.Error().Err(err).Str("channel", msg.Channel).Msg("nexusd: event bus: unmarshal delta")
					continue
				}
				srv.RelayDelta(sessionID, d)
				continue
			}
			var wire rest.EventBusMessage
			if err := json.Unmarshal([]byte(msg.Payload), &wire); err != nil {
				log.Error().Err(err).Str("channel", msg.Channel).Msg("nexusd: event bus: unmarshal event")
				continue
			}
			var evErr error
			if wire.Err != "" {
				evErr = errors.New(wire.Err)
			}
			if wire.Event != nil {
				srv.RelayEvent(sessionID, *wire.Event, evErr)
			}
		}
	}()
	return func() { _ = sub.Close() }
}

// subscribeRunEvents opens a per-session Redis Pub/Sub subscription and
// forwards every event published on it into the returned channel, closing
// the channel the moment a terminal event or an error arrives.
//
// internal/surfaces/rest's own SSE path doesn't need this — it already has
// startEventBus's single per-process PSUBSCRIBE feeding its broker
// directly, with Postgres history replay as a backstop for anything
// published before a client subscribed. telegram/zalo/email have no
// broker or replay of their own (surfaces_phase11.go's own doc comment:
// RunStarter/Resumer's contract has always been "the returned channel
// carries every event of the run, closed when it ends") — this is their
// one-subscription-per-run equivalent, so that contract still holds now
// that the run itself executes on a queue worker, possibly in a different
// process, rather than a local goroutine.
//
// Receive is called once, synchronously, before this returns: Redis
// Pub/Sub has no history, so a message published between "the caller
// enqueues/appends the job whose events we're about to listen for" and
// "the SUBSCRIBE actually reaches Redis" would otherwise be missed
// forever — callers MUST establish this subscription before triggering
// that job, the same "subscribe before replay" ordering
// internal/surfaces/rest's own handleEvents already uses.
//
// The returned cancel func MUST be called if a caller ends up never
// draining the channel to its natural end (a terminal event or an error) —
// e.g. because the job this was subscribed in anticipation of never
// actually got enqueued/appended. Without it, this goroutine blocks
// forever on a message that will now never arrive, leaking both the
// goroutine and its Redis Pub/Sub connection. The success path (draining
// to a terminal event or error) already triggers this internally and
// calling it again is a harmless no-op.
func subscribeRunEvents(ctx context.Context, redisClient *redis.Client, sessionID uuid.UUID) (events <-chan rest.RunEvent, cancel func()) {
	sub := redisClient.Subscribe(ctx, rest.EventsChannel(sessionID))
	if _, err := sub.Receive(ctx); err != nil {
		log.Error().Err(err).Any("session_id", sessionID).Msg("nexusd: subscribe to run events")
	}
	out := make(chan rest.RunEvent, 16)
	go func() {
		defer close(out)
		defer func() { _ = sub.Close() }()
		for msg := range sub.Channel() {
			var wire rest.EventBusMessage
			if err := json.Unmarshal([]byte(msg.Payload), &wire); err != nil {
				log.Error().Err(err).Any("session_id", sessionID).Msg("nexusd: run events: unmarshal")
				continue
			}
			var evErr error
			if wire.Err != "" {
				evErr = errors.New(wire.Err)
			}
			if wire.Event == nil {
				continue
			}
			out <- rest.RunEvent{Event: *wire.Event, Err: evErr}
			if evErr != nil || wire.Event.Type == store.EventTerminal {
				return
			}
		}
	}()
	return out, func() { _ = sub.Close() }
}

// resumeConversationEvents is telegram/zalo/email's shared
// ResumeConversation implementation — every one of them, unlike REST's own
// nexusdRunCtlPort.ResumeConversation, needs the full "every event until
// terminal" channel their own RunEvent-draining reply logic
// (webhook.go's own drainAndNotify) has always relied on. Subscribing
// before calling ctl.ResumeConversation (which durably appends the
// message and enqueues the continuation, internal/runctl.Control.
// ResumeConversation's own doc comment) is what keeps this race-free.
func resumeConversationEvents(ctx context.Context, redisClient *redis.Client, ctl *runctl.Control, tenantID, sessionID uuid.UUID, input string) (<-chan rest.RunEvent, error) {
	busCh, cancelBus := subscribeRunEvents(ctx, redisClient, sessionID)
	ev, err := ctl.ResumeConversation(ctx, tenantID, sessionID, input)
	if err != nil {
		cancelBus() // nothing was ever appended/enqueued for this subscription to observe — don't leak it
		return nil, err
	}
	out := make(chan rest.RunEvent, 8)
	go func() {
		defer close(out)
		out <- rest.RunEvent{Event: ev}
		for re := range busCh {
			out <- re
			if re.Err != nil || re.Event.Type == store.EventTerminal {
				return
			}
		}
	}()
	return out, nil
}
