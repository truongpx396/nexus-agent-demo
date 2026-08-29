package hooks

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// fakeHandler lets tests script a hook's response deterministically —
// scripted per call index, since a dispatch may invoke the same Kind
// multiple times with different Configs.
type fakeHandler struct {
	outcomes []Outcome
	errs     []error
	delay    time.Duration
	calls    int
}

func (f *fakeHandler) Run(ctx context.Context, _ Config, _ Context) (Outcome, error) {
	i := f.calls
	f.calls++
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	}
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	var out Outcome
	if i < len(f.outcomes) {
		out = f.outcomes[i]
	}
	return out, err
}

func newTestDispatcher(h *fakeHandler) *Dispatcher {
	d := NewDispatcher()
	d.Handlers[KindPrompt] = h
	return d
}

func TestDispatch_NoMatchingHooksDefers(t *testing.T) {
	d := newTestDispatcher(&fakeHandler{})
	out := d.Dispatch(context.Background(), PreToolUse, Context{ToolID: "platform/shell@v1", Namespace: "platform"}, nil)
	if out.Decision != Defer {
		t.Fatalf("Decision = %v, want Defer", out.Decision)
	}
}

func TestDispatch_DenyIsFinalAndShortCircuits(t *testing.T) {
	h := &fakeHandler{outcomes: []Outcome{{Decision: Deny, Reason: "first hook denies"}, {Decision: Ask}}}
	d := newTestDispatcher(h)
	configs := []Config{
		{Name: "first", Event: PreToolUse, Kind: KindPrompt, Matcher: "*"},
		{Name: "second", Event: PreToolUse, Kind: KindPrompt, Matcher: "*"},
	}
	out := d.Dispatch(context.Background(), PreToolUse, Context{ToolID: "x/y@v1"}, configs)
	if out.Decision != Deny {
		t.Fatalf("Decision = %v, want Deny", out.Decision)
	}
	if h.calls != 1 {
		t.Fatalf("calls = %d, want 1 (chain must stop at the first Deny)", h.calls)
	}
}

// TestDispatch_HookAllowIsCoercedToDefer is the Phase 3 acceptance test
// README.md §5 names verbatim: "a hook returning ALLOW is treated as DEFER."
func TestDispatch_HookAllowIsCoercedToDefer(t *testing.T) {
	h := &fakeHandler{outcomes: []Outcome{{Decision: Allow, Reason: "a buggy hook thinks it can grant permission"}}}
	d := newTestDispatcher(h)
	configs := []Config{{Name: "buggy", Event: PreToolUse, Kind: KindPrompt, Matcher: "*"}}
	out := d.Dispatch(context.Background(), PreToolUse, Context{ToolID: "x/y@v1"}, configs)
	if out.Decision != Defer {
		t.Fatalf("Decision = %v, want Defer (Allow must never survive normalize)", out.Decision)
	}
}

func TestDispatch_UnrecognizedDecisionFailsClosedToDeny(t *testing.T) {
	h := &fakeHandler{outcomes: []Outcome{{Decision: "yes_please"}}}
	d := newTestDispatcher(h)
	configs := []Config{{Name: "broken", Event: PreToolUse, Kind: KindPrompt, Matcher: "*"}}
	out := d.Dispatch(context.Background(), PreToolUse, Context{ToolID: "x/y@v1"}, configs)
	if out.Decision != Deny {
		t.Fatalf("Decision = %v, want Deny", out.Decision)
	}
}

func TestDispatch_EventAndMatcherFiltering(t *testing.T) {
	h := &fakeHandler{outcomes: []Outcome{{Decision: Deny}}}
	d := newTestDispatcher(h)
	configs := []Config{
		{Name: "wrong-event", Event: PostToolUse, Kind: KindPrompt, Matcher: "*"},
		{Name: "wrong-tool", Event: PreToolUse, Kind: KindPrompt, Matcher: "other/*"},
	}
	out := d.Dispatch(context.Background(), PreToolUse, Context{ToolID: "platform/shell@v1", Namespace: "platform"}, configs)
	if out.Decision != Defer {
		t.Fatalf("Decision = %v, want Defer (neither config should have matched)", out.Decision)
	}
	if h.calls != 0 {
		t.Fatalf("calls = %d, want 0", h.calls)
	}
}

func TestDispatch_TimeoutFailsClosedToDeny(t *testing.T) {
	h := &fakeHandler{delay: 50 * time.Millisecond}
	d := newTestDispatcher(h)
	configs := []Config{{Name: "slow", Event: PreToolUse, Kind: KindPrompt, Matcher: "*", Timeout: 5 * time.Millisecond}}
	out := d.Dispatch(context.Background(), PreToolUse, Context{ToolID: "x/y@v1"}, configs)
	if out.Decision != Deny {
		t.Fatalf("Decision = %v, want Deny (default block on timeout)", out.Decision)
	}
}

func TestDispatch_PerTurnCap(t *testing.T) {
	h := &fakeHandler{outcomes: []Outcome{{Decision: Defer}, {Decision: Defer}, {Decision: Defer}}}
	d := newTestDispatcher(h)
	d.PerTurnCap = 2
	configs := []Config{
		{Name: "a", Event: PreToolUse, Kind: KindPrompt, Matcher: "*"},
		{Name: "b", Event: PreToolUse, Kind: KindPrompt, Matcher: "*"},
		{Name: "c", Event: PreToolUse, Kind: KindPrompt, Matcher: "*"},
	}
	out := d.Dispatch(context.Background(), PreToolUse, Context{ToolID: "x/y@v1"}, configs)
	if out.Decision != Deny {
		t.Fatalf("Decision = %v, want Deny (per-turn cap exceeded on the 3rd hook)", out.Decision)
	}
	if h.calls != 2 {
		t.Fatalf("calls = %d, want 2 (the 3rd hook must never run)", h.calls)
	}
}

func TestDispatch_ChainBudget(t *testing.T) {
	h := &fakeHandler{delay: 10 * time.Millisecond, outcomes: []Outcome{{Decision: Defer}, {Decision: Defer}}}
	d := newTestDispatcher(h)
	d.ChainBudget = 5 * time.Millisecond
	configs := []Config{
		{Name: "a", Event: PreToolUse, Kind: KindPrompt, Matcher: "*", Timeout: time.Second},
		{Name: "b", Event: PreToolUse, Kind: KindPrompt, Matcher: "*", Timeout: time.Second},
	}
	out := d.Dispatch(context.Background(), PreToolUse, Context{ToolID: "x/y@v1"}, configs)
	if out.Decision != Deny {
		t.Fatalf("Decision = %v, want Deny (chain budget exceeded before the 2nd hook)", out.Decision)
	}
	if h.calls != 1 {
		t.Fatalf("calls = %d, want 1", h.calls)
	}
}

func TestDispatch_DecisionCacheMemoizesIdenticalInput(t *testing.T) {
	h := &fakeHandler{outcomes: []Outcome{{Decision: Ask, Reason: "first"}}}
	d := newTestDispatcher(h)
	configs := []Config{{Name: "cached", Event: PreToolUse, Kind: KindPrompt, Matcher: "*"}}
	input := json.RawMessage(`{"cmd":"ls"}`)

	first := d.Dispatch(context.Background(), PreToolUse, Context{ToolID: "x/y@v1", Input: input}, configs)
	second := d.Dispatch(context.Background(), PreToolUse, Context{ToolID: "x/y@v1", Input: input}, configs)
	if first.Decision != Ask || second.Decision != Ask {
		t.Fatalf("Decision = %v/%v, want Ask/Ask", first.Decision, second.Decision)
	}
	if h.calls != 1 {
		t.Fatalf("calls = %d, want 1 (second dispatch should hit the decision cache)", h.calls)
	}
}

func TestDispatch_UpdatedInputWithinAllowlistPassesThrough(t *testing.T) {
	updated := json.RawMessage(`{"path":"/workspace/new.txt"}`)
	h := &fakeHandler{outcomes: []Outcome{{Decision: Defer, UpdatedInput: updated}}}
	d := newTestDispatcher(h)
	configs := []Config{{Name: "rewriter", Event: PreToolUse, Kind: KindPrompt, Matcher: "*", UpdatablePaths: []string{"path"}}}

	out := d.Dispatch(context.Background(), PreToolUse, Context{ToolID: "x/y@v1", Input: json.RawMessage(`{"path":"/workspace/old.txt"}`)}, configs)
	if out.Decision != Defer {
		t.Fatalf("Decision = %v, want Defer", out.Decision)
	}
	if string(out.UpdatedInput) != string(updated) {
		t.Fatalf("UpdatedInput = %s, want %s", out.UpdatedInput, updated)
	}
}

func TestDispatch_UpdatedInputOutsideAllowlistIsRefused(t *testing.T) {
	updated := json.RawMessage(`{"path":"/etc/passwd"}`)
	h := &fakeHandler{outcomes: []Outcome{{Decision: Defer, UpdatedInput: updated}}}
	d := newTestDispatcher(h)
	// Note: no UpdatablePaths declared — must refuse the rewrite outright.
	configs := []Config{{Name: "rewriter", Event: PreToolUse, Kind: KindPrompt, Matcher: "*"}}

	out := d.Dispatch(context.Background(), PreToolUse, Context{ToolID: "x/y@v1", Input: json.RawMessage(`{"path":"/workspace/old.txt"}`)}, configs)
	if out.Decision != Deny {
		t.Fatalf("Decision = %v, want Deny (a rewrite with no declared allowlist must be refused)", out.Decision)
	}
	if out.UpdatedInput != nil {
		t.Fatalf("UpdatedInput = %s, want nil (a refused rewrite must not be applied)", out.UpdatedInput)
	}
}

func TestDispatch_UpdatedInputTouchingDisallowedPathIsRefused(t *testing.T) {
	updated := json.RawMessage(`{"path":"/workspace/new.txt","recursive":true}`)
	h := &fakeHandler{outcomes: []Outcome{{Decision: Defer, UpdatedInput: updated}}}
	d := newTestDispatcher(h)
	configs := []Config{{Name: "rewriter", Event: PreToolUse, Kind: KindPrompt, Matcher: "*", UpdatablePaths: []string{"path"}}}

	out := d.Dispatch(context.Background(), PreToolUse, Context{ToolID: "x/y@v1", Input: json.RawMessage(`{"path":"/workspace/old.txt"}`)}, configs)
	if out.Decision != Deny {
		t.Fatalf("Decision = %v, want Deny ('recursive' is outside the declared allowlist)", out.Decision)
	}
}
