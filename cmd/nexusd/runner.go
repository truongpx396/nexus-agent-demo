package main

import (
	"context"
	"fmt"

	"github.com/truongpx396/nexus-agent-demo/internal/memory"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// kernelRunStarter is the only implementation of rest.RunStarter this binary
// ships, and the only place a kernel.RunState/kernel.RunConfig gets built —
// internal/surfaces/rest never constructs either directly (starter.go's doc
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
}

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

	ch := make(chan rest.RunEvent, 8)
	go func() {
		defer close(ch)
		// A run outlives the HTTP request that started it — the client
		// already has its 202 and may not even open the SSE stream.
		for ev, err := range a.kernel.Run(context.Background(), st, cfg) {
			ch <- rest.RunEvent{Event: ev, Err: err}
			if err != nil {
				return
			}
		}
	}()
	return ch, nil
}
