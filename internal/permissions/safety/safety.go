// Package safety is Gate 3 of the permission chain (README task 3.9,
// pattern 20): a per-invocation hybrid classifier evaluated on PARSED input,
// never on the tool name alone (constitution Principle V — `Bash("ls")` !=
// `Bash("rm -rf")`). A deterministic, in-process rule pass runs first and can
// resolve Deny or Ask outright; anything the rules don't recognize escalates
// to a model leg with a bounded timeout and a circuit breaker, which fails
// closed to Ask (never to Allow, and never to Deny — an outage is not itself
// evidence of danger) when it errors, times out, or the breaker is open.
//
// This package defines its own Verdict rather than importing
// internal/permissions's Decision — the same "duplicate the shape, skip the
// import" seam internal/surfaces/rest uses for kernel.SealFunc — so
// internal/permissions can import this package (chain.go calls Classify
// directly for layer 6) without a cycle.
package safety

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"time"
)

// Verdict is this package's local decision vocabulary. Allow deliberately
// does not exist: a per-invocation safety classifier is not a source of
// permission, only ever a reason to stop or ask (mirrors Gate 2/3's "never
// ALLOW" rule in README.md §4's chain table).
type Verdict string

const (
	VerdictDeny  Verdict = "deny"
	VerdictAsk   Verdict = "ask"
	VerdictDefer Verdict = "defer"
)

// Result is Classify's return: a Verdict plus the reason an auditor (or the
// permission chain's caller) needs to understand why.
type Result struct {
	Verdict Verdict
	Reason  string
}

// Rule is one deterministic pattern in the rule pass. Pattern is matched
// against "<tool_id> <raw input JSON>" — matching the actual invocation
// text, not a static per-tool label, is what makes this per-invocation
// rather than per-tool.
type Rule struct {
	Name    string
	Pattern *regexp.Regexp
	Verdict Verdict // Deny or Ask only — a rule that only ever clears input belongs in DefaultRules' absence, not as a Defer rule
	Reason  string
}

// DefaultRules is a small, deliberately narrow starting set: catastrophic,
// unambiguous patterns deny outright; broad-but-not-always-dangerous ones
// ask. This is demo-scoped illustrative policy, not a security product —
// internal/permissions.ChainConfig lets a caller supply its own set.
func DefaultRules() []Rule {
	return []Rule{
		{
			Name:    "rm_rf_root",
			Pattern: regexp.MustCompile(`rm\s+(-\w*r\w*f\w*|-\w*f\w*r\w*)\s+/(\s|$|["'])`),
			Verdict: VerdictDeny,
			Reason:  "matches an unrecoverable root-filesystem delete pattern",
		},
		{
			Name:    "fork_bomb",
			Pattern: regexp.MustCompile(`:\(\)\s*\{\s*:\|:&\s*\}\s*;\s*:`),
			Verdict: VerdictDeny,
			Reason:  "matches a fork-bomb pattern",
		},
		{
			Name:    "drop_table",
			Pattern: regexp.MustCompile(`(?i)drop\s+table\b`),
			Verdict: VerdictDeny,
			Reason:  "matches a destructive DDL statement",
		},
		{
			Name:    "pipe_to_shell",
			Pattern: regexp.MustCompile(`curl[^|]*\|\s*(sudo\s+)?(sh|bash)\b`),
			Verdict: VerdictAsk,
			Reason:  "downloads and executes a remote script unreviewed",
		},
		{
			Name:    "sudo",
			Pattern: regexp.MustCompile(`\bsudo\b`),
			Verdict: VerdictAsk,
			Reason:  "escalates privileges",
		},
	}
}

// ModelClassifier is the "model leg": a bounded second opinion for input the
// rule pass didn't recognize. internal/provider.Provider is not referenced
// here on purpose — this package stays a leaf; the leg is a narrow
// text-in/Verdict-out seam a caller wires to whatever it likes (a cheap
// model call through the ordinary Provider port, in production).
type ModelClassifier interface {
	Classify(ctx context.Context, toolID string, input string) (Verdict, string, error)
}

// NoModelClassifier is the zero value's classifier: always errors, so
// Classify's fail-closed-to-Ask path is exercised until a real model leg is
// configured. This is the Phase 3 default — deliberately conservative,
// exactly like kernel.NotImplementedToolExecutor was Phase 2's.
type NoModelClassifier struct{}

func (NoModelClassifier) Classify(context.Context, string, string) (Verdict, string, error) {
	return "", "", errors.New("safety: no model classifier configured")
}

// breaker is a minimal consecutive-failure circuit breaker: three identical
// failures in a row (constitution Principle VIII's own threshold) opens the
// breaker for Cooldown; while open, Classify skips straight to the
// fail-closed path without calling Model at all.
type breaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
	threshold int
	cooldown  time.Duration
}

func (b *breaker) open(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return now.Before(b.openUntil)
}

func (b *breaker) recordFailure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= b.threshold {
		b.openUntil = now.Add(b.cooldown)
		b.failures = 0
	}
}

func (b *breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
}

// Classifier is Gate 3's entry point: NewClassifier(rules, model, timeout)
// then Classify per invocation.
type Classifier struct {
	Rules   []Rule
	Model   ModelClassifier
	Timeout time.Duration // bounded timeout for the model leg (README task 3.9)

	breaker *breaker
	now     func() time.Time // overridable for tests
}

// NewClassifier builds a Classifier. A nil Model is replaced with
// NoModelClassifier so callers never need a nil check.
func NewClassifier(rules []Rule, model ModelClassifier, timeout time.Duration) *Classifier {
	if model == nil {
		model = NoModelClassifier{}
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Classifier{
		Rules:   rules,
		Model:   model,
		Timeout: timeout,
		breaker: &breaker{threshold: 3, cooldown: 30 * time.Second},
		now:     time.Now,
	}
}

// Classify runs the rule pass, then (only if the rules found nothing) the
// bounded model leg. A model-leg error, timeout, or open breaker resolves
// Ask — never Deny, never Allow (Allow does not exist in this vocabulary):
// an unavailable classifier is a reason to ask a human, not a reason to
// assume danger or to assume safety.
func (c *Classifier) Classify(ctx context.Context, toolID string, rawInput string) Result {
	text := toolID + " " + rawInput
	for _, r := range c.Rules {
		if r.Pattern.MatchString(text) {
			return Result{Verdict: r.Verdict, Reason: "rule:" + r.Name + ": " + r.Reason}
		}
	}

	if c.breaker.open(c.now()) {
		return Result{Verdict: VerdictAsk, Reason: "safety model leg circuit breaker open; failing closed to ask"}
	}

	cctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	type outcome struct {
		verdict Verdict
		reason  string
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		v, reason, err := c.Model.Classify(cctx, toolID, rawInput)
		done <- outcome{v, reason, err}
	}()

	select {
	case <-cctx.Done():
		c.breaker.recordFailure(c.now())
		return Result{Verdict: VerdictAsk, Reason: "safety model leg timed out; failing closed to ask"}
	case o := <-done:
		if o.err != nil {
			c.breaker.recordFailure(c.now())
			return Result{Verdict: VerdictAsk, Reason: "safety model leg unavailable (" + o.err.Error() + "); failing closed to ask"}
		}
		c.breaker.recordSuccess()
		return Result{Verdict: o.verdict, Reason: o.reason}
	}
}
