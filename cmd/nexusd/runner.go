package main

import (
	"context"
	"fmt"
	"iter"

	"github.com/redis/go-redis/v9"

	"github.com/truongpx396/nexus-agent/internal/eventlog"
	"github.com/truongpx396/nexus-agent/internal/memory"
	"github.com/truongpx396/nexus-agent/internal/provider"
	"github.com/truongpx396/nexus-agent/internal/queue"
	"github.com/truongpx396/nexus-agent/internal/runs"
	"github.com/truongpx396/nexus-agent/kernel"
)

// kernelRunStarter is the only implementation of runs.Starter this
// binary ships, and the only place a kernel.RunState/kernel.RunConfig gets
// built — internal/surfaces/rest never constructs either directly (internal/runs's
// package comment).
type kernelRunStarter struct {
	kernel      *kernel.Kernel
	system      string
	catalog     []provider.ToolSchema
	loadedTools []string
	maxTurns    int

	// memory backs README task 7.1's "injected at session start": a nil
	// memory leaves cfg.MemoryText and cfg.MemorySources empty, exactly the
	// pre-Phase-7 behavior every earlier test still gets.
	memory *memory.Store

	// queue is where StartRun enqueues the queue.KindStart job that
	// actually drives the turn loop, once Seed has durably appended the
	// run's own opening event — the same split internal/runctl.Control.
	// ResumeConversation now uses for a conversational follow-up.
	queue queue.Port

	// redisClient backs subscribeRunEvents (eventbus.go) — StartRun
	// subscribes to the run's own bus channel BEFORE Seed/Enqueue below, so
	// every surface that shares this starter (REST, telegram, zalo, email,
	// cron) keeps getting RunStarter's documented contract — "a channel of
	// every event the run produces, closed when the run ends" — even
	// though the turn loop itself now executes on a queue worker, possibly
	// a different process, rather than a goroutine this call started.
	redisClient *redis.Client
}

// seedHeadroom is the room drainSeed leaves in the channel beyond the seed
// itself, for events the forwarding goroutine relays before the caller
// starts reading.
const seedHeadroom = 8

// drainSeed collects everything a Kernel.Seed sequence yields into a channel
// sized to hold all of it, and reports whether the sequence ended in an error
// (that error is the last event in the channel).
//
// Seed runs inline, before StartRun has returned the channel to anyone, so
// nothing is receiving yet. Seed yields one EventToolLoaded per resident
// tool plus the memory and user-message events, so a fixed buffer smaller
// than that blocks the send forever and hangs the HTTP request that asked
// for the run — the default catalog seeds 15 events, and the buffer used to
// be 8.
func drainSeed(seq iter.Seq2[eventlog.Event, error]) (ch chan runs.Event, failed bool) {
	var seeded []runs.Event
	for ev, err := range seq {
		seeded = append(seeded, runs.Event{Event: ev, Err: err})
		if err != nil {
			failed = true
			break
		}
	}
	ch = make(chan runs.Event, len(seeded)+seedHeadroom)
	for _, re := range seeded {
		ch <- re
	}
	return ch, failed
}

// StartRun seeds a fresh run synchronously — Kernel.Seed's own status
// flip and pre-turn-loop events (EventToolLoaded*, EventMemoryLoaded?,
// EventUserMessage) are a couple of cheap, deterministic Postgres writes,
// the same cost class as the session+DEK writes internal/surfaces/rest's
// handleCreateRun already does inline — then enqueues a queue.KindStart
// job carrying only ids, never req.Input's own plaintext. Whichever worker
// picks that job up (internal/queue's pool, cmd/nexusd's queueRunner)
// drives the actual turn loop via internal/runctl.Control.Resume ->
// kernel.Kernel.Continue: the same "rehydrate arbitrary existing history,
// then continue" path a crash-recovered session already uses.
func (a *kernelRunStarter) StartRun(ctx context.Context, req runs.Request) (<-chan runs.Event, error) {
	// The tenant's memory and a run's own MCP tools are not folded into the
	// process-wide system prompt and catalog here. They go on the config as
	// MemoryText and ExtraCatalog, which Seed records in the session's opening
	// events; the worker that drives the turns reads them back from the log
	// (kernel.RehydratePinned), so the model sees what the session started with
	// whichever process runs it. Folding them in here changed only this call's
	// config, which never reaches the model.
	var memoryText string
	var memorySources []string
	if a.memory != nil {
		snap, err := a.memory.Load(ctx, req.TenantID)
		if err != nil {
			return nil, fmt.Errorf("load memory for tenant %s: %w", req.TenantID, err)
		}
		memoryText = snap.Text
		memorySources = snap.SourceIDs
	}

	// Task 11.1: a run's own MCP tool ids (runs.Request's own doc comment) are
	// appended to the process-wide base list here — additive, the same shape
	// memorySources already is for a per-run addition to a base config. Every
	// pre-Phase-11 caller leaves it nil, so append is a no-op and this path is
	// unchanged for them.
	loadedTools := a.loadedTools
	if len(req.ExtraLoadedTools) > 0 {
		loadedTools = append(append([]string{}, a.loadedTools...), req.ExtraLoadedTools...)
	}

	st := &kernel.RunState{
		TenantID:  req.TenantID,
		SessionID: req.SessionID,
		Seal:      kernel.SealFunc(req.Seal),
	}
	cfg := kernel.RunConfig{
		System:         a.system,
		Catalog:        a.catalog,
		LoadedTools:    loadedTools,
		MemorySources:  memorySources,
		MemoryText:     memoryText,
		ExtraCatalog:   req.ExtraCatalog,
		ModelID:        req.ModelID,
		MaxTurns:       a.maxTurns,
		Input:          req.Input,
		AutonomyLevel:  req.AutonomyLevel,
		Conversational: req.Conversational,
	}

	// Subscribed BEFORE Seed/Enqueue below — Redis Pub/Sub has no history,
	// so a message the eventual worker publishes between "this call
	// enqueues the job" and "somebody subscribes" would otherwise be missed
	// forever (subscribeRunEvents' own doc comment, eventbus.go). cancelBus
	// MUST run on every early-return path below (Seed/Enqueue failing
	// means nothing was ever appended/enqueued for it to observe) or its
	// goroutine blocks forever on a message that will now never arrive.
	busCh, cancelBus := subscribeRunEvents(ctx, a.redisClient, req.SessionID)

	// Seed runs on THIS goroutine, inline with the HTTP request that asked
	// for it — unlike the old full a.kernel.Run(...), it's only a handful
	// of durable writes, not the turn loop itself, so there is no "a run
	// outlives the HTTP request" concern to defer it into a goroutine for.
	ch, seedFailed := drainSeed(a.kernel.Seed(ctx, st, cfg))
	if seedFailed {
		cancelBus()
		close(ch)
		return ch, nil
	}

	if _, err := a.queue.Enqueue(ctx, queue.Job{
		TenantID: req.TenantID, SessionID: req.SessionID, SessionKey: req.SessionID.String(), Kind: queue.KindStart,
	}); err != nil {
		cancelBus()
		close(ch)
		return nil, fmt.Errorf("enqueue start job for session %s: %w", req.SessionID, err)
	}

	// A run outlives the HTTP request that started it — the client already
	// has its 202 and may not even open the SSE stream. Whichever worker
	// (in whichever process) leases the job just enqueued drives the real
	// turn loop and publishes its events onto busCh; this goroutine's only
	// job is forwarding them until the run's actual terminal event arrives.
	go func() {
		defer close(ch)
		for re := range busCh {
			ch <- re
			if re.Err != nil || re.Event.Type == eventlog.EventTerminal {
				return
			}
		}
	}()
	return ch, nil
}
