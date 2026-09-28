package main

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/truongpx396/nexus-agent-demo/internal/memory"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/queue"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// kernelRunStarter is the only implementation of rest.RunStarter this
// binary ships, and the only place a kernel.RunState/kernel.RunConfig gets
// built — internal/surfaces/rest never constructs either directly (starter.go's doc
// comment).
type kernelRunStarter struct {
	kernel      *kernel.Kernel
	system      string
	catalog     []provider.ToolSchema
	loadedTools []string
	maxTurns    int

	// memory/store back README task 7.1's "injected at session start": a
	// nil memory leaves cfg.System untouched and cfg.MemorySources empty,
	// exactly the pre-Phase-7 behavior every earlier test still gets.
	memory *memory.Store
	store  *store.Store

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
func (a *kernelRunStarter) StartRun(ctx context.Context, req rest.RunRequest) (<-chan rest.RunEvent, error) {
	system := a.system
	var memorySources []string
	if a.memory != nil && a.store != nil {
		snap, err := a.memory.LoadForSession(ctx, a.store, req.TenantID)
		if err != nil {
			return nil, fmt.Errorf("load memory for tenant %s: %w", req.TenantID, err)
		}
		if snap.Text != "" {
			system = system + "\n\n" + snap.Text
		}
		memorySources = snap.SourceIDs
	}

	// Task 11.1: a run's own MCP catalog addition (rest.RunRequest's own
	// doc comment) is appended to the process-wide base catalog/loadedTools
	// here — additive, the same shape memorySources already is for a
	// per-run addition to a base config. Every pre-Phase-11 caller leaves
	// both nil, so append is a no-op and this path is unchanged for them.
	catalog := a.catalog
	loadedTools := a.loadedTools
	if len(req.ExtraCatalog) > 0 {
		catalog = append(append([]provider.ToolSchema{}, a.catalog...), req.ExtraCatalog...)
		loadedTools = append(append([]string{}, a.loadedTools...), req.ExtraLoadedTools...)
	}

	st := &kernel.RunState{
		TenantID:  req.TenantID,
		SessionID: req.SessionID,
		Seal:      kernel.SealFunc(req.Seal),
	}
	cfg := kernel.RunConfig{
		System:         system,
		Catalog:        catalog,
		LoadedTools:    loadedTools,
		MemorySources:  memorySources,
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

	ch := make(chan rest.RunEvent, 8)
	// Seed runs on THIS goroutine, inline with the HTTP request that asked
	// for it — unlike the old full a.kernel.Run(...), it's only a handful
	// of durable writes, not the turn loop itself, so there is no "a run
	// outlives the HTTP request" concern to defer it into a goroutine for.
	for ev, err := range a.kernel.Seed(ctx, st, cfg) {
		ch <- rest.RunEvent{Event: ev, Err: err}
		if err != nil {
			cancelBus()
			close(ch)
			return ch, nil
		}
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
			if re.Err != nil || re.Event.Type == store.EventTerminal {
				return
			}
		}
	}()
	return ch, nil
}
