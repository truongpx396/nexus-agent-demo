package kernel_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/promptctx"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// reserveRecorder is a BudgetGate that remembers every reservation it is asked for.
type reserveRecorder struct {
	mu   sync.Mutex
	reqs []cost.ReserveRequest
}

func (g *reserveRecorder) Reserve(ctx context.Context, req cost.ReserveRequest) (cost.Reservation, error) {
	g.mu.Lock()
	g.reqs = append(g.reqs, req)
	g.mu.Unlock()
	return kernel.NoopBudgetGate{}.Reserve(ctx, req)
}

func (*reserveRecorder) Reconcile(context.Context, cost.Reservation, provider.Usage, bool) error {
	return nil
}

func (g *reserveRecorder) byPurpose(p cost.Purpose) []cost.ReserveRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []cost.ReserveRequest
	for _, r := range g.reqs {
		if r.Purpose == p {
			out = append(out, r)
		}
	}
	return out
}

// The turn reserves for the prompt it will send: the system prompt, the catalog and the
// whole transcript so far, so a long history is reserved at its own size and not at a
// fixed 8,000 tokens.
func TestRun_EachTurnReservesForThePromptItSends(t *testing.T) {
	rec := &reserveRecorder{}
	r := newRig(toolFunc(func(kernel.ToolUseRequest) kernel.ToolResult {
		return kernel.ToolResult{Output: []byte(`"` + strings.Repeat("o", 3000) + `"`)}
	}), toolTurn, finalTurn)
	r.k.Budget = rec

	input := strings.Repeat("i", 90_000)
	cfg := kernel.RunConfig{
		System: "test", ModelID: "fake", MaxTurns: 5, Input: input, AutonomyLevel: "supervised",
		Catalog: []provider.ToolSchema{{Name: "platform/echo@v1", Description: "echo"}},
	}
	for _, err := range r.k.Run(context.Background(), r.st, cfg) {
		if err != nil {
			t.Fatal(err)
		}
	}

	turns := rec.byPurpose(cost.PurposeTurn)
	if len(turns) != 2 {
		t.Fatalf("%d turn reservations, want 2", len(turns))
	}
	wantFirst := promptctx.EstimateTokens("test", cfg.Catalog, []provider.Message{provider.TextMessage("user", input)})
	if turns[0].InputTokenEstimate != wantFirst || wantFirst < 30_000 {
		t.Errorf("first turn estimate = %d, want %d (about a third of the 90 KB prompt)", turns[0].InputTokenEstimate, wantFirst)
	}
	if turns[1].InputTokenEstimate <= turns[0].InputTokenEstimate {
		t.Errorf("second turn estimate %d did not grow past the first %d: the history now holds the tool call and its 3 KB result", turns[1].InputTokenEstimate, turns[0].InputTokenEstimate)
	}
}

// The summariser is sent the covered messages, so its reservation is sized from the
// prompt it gets, not the fixed estimate.
func TestCondense_ReservesForThePromptTheSummariserIsSent(t *testing.T) {
	rec := &reserveRecorder{}
	condensingRigWith(t, rec)

	compaction := rec.byPurpose(cost.PurposeCompaction)
	if len(compaction) != 1 {
		t.Fatalf("%d compaction reservations, want 1", len(compaction))
	}
	// The covered messages include the first call's 600-byte result, which the summariser is shown whole.
	if got := compaction[0].InputTokenEstimate; got < 200 {
		t.Errorf("compaction estimate = %d, want it sized from the covered messages (at least 200 tokens)", got)
	}
	if got := compaction[0].ModelID; got != "cheap" {
		t.Errorf("compaction reserved under %q, want the condenser model", got)
	}
}
