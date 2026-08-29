// Package kernel is THE loop (docs/constitution.md, Principle I): the only
// place control flow lives, expressed as a single generator
// (kernel/loop.go). It may import internal/{provider,tools,promptctx,store,
// cost,reliability,obs} and nothing else — tests/contract/boundaries_test.go
// enforces that this stays true even after later phases add the packages
// this file already seams for.
package kernel

import (
	"context"
	"encoding/json"

	"github.com/truongpx396/nexus-agent-demo/internal/provider"
)

// ToolUseRequest is the kernel's view of one tool_use assembled from a
// provider turn — enough to append an EventToolUse and hand to a
// ToolExecutor.
type ToolUseRequest struct {
	ToolUseID string
	ToolName  string
	Input     json.RawMessage
}

// ToolResult is what a ToolExecutor returns for one ToolUseRequest.
type ToolResult struct {
	Output    json.RawMessage
	IsError   bool
	Reason    string // set when IsError, or to explain a Synthetic result
	Synthetic bool
}

// ToolExecutor runs one tool_use to completion. internal/tools/pipeline.go
// (Phase 3, README task 3.4) is the real implementation; this seam exists so
// the loop's dispatch step compiles and appends a correctly paired result
// now, even though nothing can actually execute a tool until Phase 3 lands.
type ToolExecutor interface {
	Execute(ctx context.Context, req ToolUseRequest) ToolResult
}

// NotImplementedToolExecutor is the only ToolExecutor Phase 2 ships. Every
// call returns a synthetic error result — the paired-result invariant holds
// even though no tool actually runs yet.
type NotImplementedToolExecutor struct{}

func (NotImplementedToolExecutor) Execute(_ context.Context, _ ToolUseRequest) ToolResult {
	return ToolResult{
		IsError:   true,
		Synthetic: true,
		Reason:    "tool pipeline not implemented until Phase 3 (internal/tools/pipeline.go)",
	}
}

// BudgetGate is consulted before every Provider.Stream call — the "reserve"
// step in README task 2.1's loop order (hygiene -> reserve -> stream -> ...).
// internal/cost/gate.go (Phase 4, README task 4.4) is the real
// reserve-then-reconcile implementation; NoopBudgetGate always allows, so the
// loop's shape is real now without cost logic existing yet.
type BudgetGate interface {
	Reserve(ctx context.Context) error
}

// NoopBudgetGate is the only BudgetGate Phase 2 ships.
type NoopBudgetGate struct{}

func (NoopBudgetGate) Reserve(_ context.Context) error { return nil }

// SealFunc seals one event's plaintext payload, returning the sealed bytes,
// a digest over the plaintext (survives crypto-shredding, FR-081), and the
// key id it was sealed under. The loop depends on this function type rather
// than internal/crypto directly, so kernel/hygiene.go and its property test
// stay free of any crypto or DB setup.
type SealFunc func(plaintext []byte) (sealed, digest []byte, keyID string, err error)

// RunConfig is everything one Run needs beyond the growing event history.
// ModelID is the internal/provider/router.go decision, persisted onto the
// session and stamped on model-produced events for audit — Phase 2 computes
// and records it but dispatches every call through the one configured
// Provider regardless of its value (per-model provider selection is later-
// phase routing infrastructure, not a Phase 2 task).
type RunConfig struct {
	System   string
	Catalog  []provider.ToolSchema
	ModelID  string
	MaxTurns int
	// Input, if non-empty, is appended as the run's opening EventUserMessage
	// before the turn loop starts. Empty for a resumed/continued run (not
	// exercised this phase — Phase 6 owns resume).
	Input string
}
