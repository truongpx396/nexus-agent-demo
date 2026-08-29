package hooks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Dispatcher runs one event's hook chain in configured order, aggregating
// every matching hook's Outcome into one final Outcome for the caller
// (internal/tools/pipeline.go's step 7). DENY from any hook is final and
// stops the chain immediately; ASK/DEFER never stop it — every remaining
// matching hook still runs, so the chain budget and per-turn cap below are
// what bound total chain cost, not early exit on a "good enough" answer.
type Dispatcher struct {
	Handlers       map[Kind]Handler
	ChainBudget    time.Duration // total wall-clock budget for one Dispatch call (default block on overrun)
	PerTurnCap     int           // max hook invocations across one kernel turn; 0 = unlimited
	DefaultTimeout time.Duration // per-hook timeout when Config.Timeout is unset (default block on timeout)

	mu        sync.Mutex
	cache     map[string]Outcome // key: hook name + ":" + sha256(input) — memoizes a hook's answer for an unchanged call within the run
	turnCount int
}

// NewDispatcher wires the three built-in handler kinds. Sensible defaults
// (5s chain budget, 8 hooks/turn, 2s per-hook timeout) are demo-scoped
// policy, not load-bearing constants — every field is exported and
// overridable by the caller that constructs one.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		Handlers: map[Kind]Handler{
			KindCommand: CommandHandler{},
			KindHTTP:    HTTPHandler{},
			KindPrompt:  PromptHandler{},
		},
		ChainBudget:    5 * time.Second,
		PerTurnCap:     8,
		DefaultTimeout: 2 * time.Second,
		cache:          map[string]Outcome{},
	}
}

// ResetTurn clears the per-turn hook-invocation counter. The kernel loop
// calls this once per turn (before dispatching any tool_use in that turn),
// so the cap bounds hook cost per turn rather than per run.
func (d *Dispatcher) ResetTurn() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.turnCount = 0
}

// blockOutcome is the fail-closed answer for anything that prevents a hook
// from running to completion (chain budget exhausted, per-turn cap hit,
// handler error, handler timeout, unrecognized decision) — "default block"
// (task 3.11) applied uniformly at every failure point in this file.
func blockOutcome(hookName, reason string) Outcome {
	return Outcome{Decision: Deny, Reason: reason, HookName: hookName}
}

// normalize enforces this package's one hard rule: Allow is not a decision a
// hook can hand back authoritatively. A raw Allow is coerced to Defer; an
// empty or unrecognized string fails closed to Deny instead, since a hook
// that returned garbage is closer to "broken" than "silent yes."
func normalize(out Outcome, hookName string) Outcome {
	switch out.Decision {
	case Deny, Ask, Defer:
		// already valid
	case Allow:
		out.Decision = Defer
		out.Reason = fmt.Sprintf("hook %s returned allow; coerced to defer — a hook can never grant permission", hookName)
	default:
		out.Decision = Deny
		if out.Reason == "" {
			out.Reason = fmt.Sprintf("hook %s returned an unrecognized decision %q; failing closed to deny", hookName, out.Decision)
		}
	}
	if out.HookName == "" {
		out.HookName = hookName
	}
	return out
}

func decisionRank(d Decision) int {
	switch d {
	case Deny:
		return 3
	case Ask:
		return 2
	case Defer:
		return 1
	case Allow:
		// unreachable: normalize already coerced this to Defer before it
		// could reach combine/decisionRank. Listed explicitly so this
		// switch stays exhaustive over the Decision type.
		return 1
	default:
		return 1
	}
}

// combine folds one hook's Outcome into the chain's running aggregate:
// DENY beats ASK beats DEFER, and a validly-rewritten input always carries
// forward regardless of which outcome "wins" the decision.
func combine(agg, out Outcome) Outcome {
	next := agg
	if decisionRank(out.Decision) > decisionRank(agg.Decision) {
		next = Outcome{Decision: out.Decision, Reason: out.Reason, HookName: out.HookName}
	}
	if out.UpdatedInput != nil {
		next.UpdatedInput = out.UpdatedInput
	}
	return next
}

func digestHex(input json.RawMessage) string {
	sum := sha256.Sum256(input)
	return hex.EncodeToString(sum[:])
}

// touchedPaths reports which top-level JSON fields differ between orig and
// updated (added, removed, or changed) — the unit enforcePathAllowlist
// checks against a hook's declared UpdatablePaths. Both must be JSON
// objects; anything else is refused, never guessed at.
func touchedPaths(orig, updated json.RawMessage) ([]string, error) {
	var o, u map[string]json.RawMessage
	if len(orig) == 0 {
		orig = []byte("{}")
	}
	if err := json.Unmarshal(orig, &o); err != nil {
		return nil, fmt.Errorf("original input is not a JSON object: %w", err)
	}
	if err := json.Unmarshal(updated, &u); err != nil {
		return nil, fmt.Errorf("updated input is not a JSON object: %w", err)
	}
	seen := map[string]bool{}
	var touched []string
	for k, v := range u {
		seen[k] = true
		if ov, ok := o[k]; !ok || !bytes.Equal(bytes.TrimSpace(ov), bytes.TrimSpace(v)) {
			touched = append(touched, k)
		}
	}
	for k := range o {
		if !seen[k] {
			touched = append(touched, k)
		}
	}
	sort.Strings(touched)
	return touched, nil
}

func pathAllowed(path string, allowlist []string) bool {
	for _, a := range allowlist {
		if a == path {
			return true
		}
	}
	return false
}

// enforcePathAllowlist is the "path allowlist that re-binds the digest"
// (task 3.11): a hook that didn't attempt a rewrite passes through
// untouched; one that did is refused outright — never partially applied —
// unless every touched top-level field is in cfg.UpdatablePaths. An empty
// allowlist refuses every rewrite, since "no declared paths" cannot mean
// "any path": the default must be fail closed.
func enforcePathAllowlist(out Outcome, original json.RawMessage, allowlist []string) Outcome {
	if out.UpdatedInput == nil {
		return out
	}
	if len(allowlist) == 0 {
		return blockOutcome(out.HookName, fmt.Sprintf("hook %s attempted to rewrite tool input with no configured allowlist; refused", out.HookName))
	}
	touched, err := touchedPaths(original, out.UpdatedInput)
	if err != nil {
		return blockOutcome(out.HookName, fmt.Sprintf("hook %s: %s", out.HookName, err))
	}
	for _, p := range touched {
		if !pathAllowed(p, allowlist) {
			return blockOutcome(out.HookName, fmt.Sprintf("hook %s attempted to rewrite input path %q, outside its allowlist %v; refused", out.HookName, p, allowlist))
		}
	}
	return out
}

// Dispatch runs every Config matching event/hctx, in order, and returns the
// chain's aggregated Outcome. hctx.Input reflects the most recent validly
// accepted rewrite by the time Dispatch returns — the caller (pipeline
// step 8) is expected to recompute the canonical digest from
// result.UpdatedInput whenever it is non-nil.
func (d *Dispatcher) Dispatch(ctx context.Context, event Event, hctx Context, configs []Config) Outcome {
	deadline := time.Now().Add(d.chainBudget())
	agg := Outcome{Decision: Defer}

	for _, cfg := range configs {
		if cfg.Event != event || !matchesTool(cfg.Matcher, hctx.ToolID, hctx.Namespace) {
			continue
		}
		if cfg.If != nil && !cfg.If.Eval(hctx.fields()) {
			continue
		}

		if time.Now().After(deadline) {
			return combine(agg, blockOutcome(cfg.Name, "hook chain budget exceeded; remaining hooks skipped, failing closed"))
		}
		if d.overPerTurnCap() {
			return combine(agg, blockOutcome(cfg.Name, "per-turn hook cap exceeded; remaining hooks skipped, failing closed"))
		}

		out := d.runCached(ctx, cfg, hctx)
		out = enforcePathAllowlist(out, hctx.Input, cfg.UpdatablePaths)
		if out.UpdatedInput != nil {
			hctx.Input = out.UpdatedInput
		}

		agg = combine(agg, out)
		if agg.Decision == Deny {
			return agg
		}
	}
	return agg
}

func (d *Dispatcher) chainBudget() time.Duration {
	if d.ChainBudget <= 0 {
		return 5 * time.Second
	}
	return d.ChainBudget
}

func (d *Dispatcher) overPerTurnCap() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.turnCount++
	return d.PerTurnCap > 0 && d.turnCount > d.PerTurnCap
}

// runCached executes cfg's handler, or returns a memoized answer for an
// identical (hook, input-digest) pair already seen this run (task 3.11's
// "decision cache"). The allowlist check happens in the caller, on every
// return path — including a cache hit — so a cache entry is always the raw
// handler outcome, never a pre-filtered one.
func (d *Dispatcher) runCached(ctx context.Context, cfg Config, hctx Context) Outcome {
	key := cfg.Name + ":" + digestHex(hctx.Input)
	d.mu.Lock()
	cached, ok := d.cache[key]
	d.mu.Unlock()
	if ok {
		return cached
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = d.DefaultTimeout
	}
	hcctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	handler, known := d.Handlers[cfg.Kind]
	var out Outcome
	if !known {
		out = blockOutcome(cfg.Name, fmt.Sprintf("no handler registered for hook kind %q", cfg.Kind))
	} else {
		raw, err := handler.Run(hcctx, cfg, hctx)
		switch {
		case hcctx.Err() != nil:
			out = blockOutcome(cfg.Name, "hook timed out; failing closed to deny")
		case err != nil:
			out = blockOutcome(cfg.Name, "hook error: "+err.Error())
		default:
			out = normalize(raw, cfg.Name)
		}
	}

	d.mu.Lock()
	d.cache[key] = out
	d.mu.Unlock()
	return out
}
