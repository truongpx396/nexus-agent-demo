// Package hooks is the hook layer (README task 3.11, pattern 22):
// pre_tool_use / post_tool_use, tighten-only. A hook can never grant
// permission — PreToolUse resolves DENY (final) | ASK | DEFER and nothing
// else; an ALLOW from a misbehaving handler is coerced to DEFER before it
// ever reaches a caller (dispatcher.go's normalize, and the Phase 3
// acceptance test named for it in README.md §5). PostToolUse is
// observe-and-tighten only: it can flag a result for review or redact it,
// never invert a failure into a success or widen anything.
package hooks

import (
	"context"
	"encoding/json"
	"time"
)

// Event is which side of a tool call a hook chain runs on.
type Event string

const (
	PreToolUse  Event = "pre_tool_use"
	PostToolUse Event = "post_tool_use"
)

// Decision is one hook's (or the aggregated chain's) resolution. Allow is
// part of the wire vocabulary — a raw command/http hook response is free to
// say "allow" — precisely so normalize has something concrete to coerce
// (see the package doc comment); nothing in this package ever treats Allow
// as authoritative.
type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
	Defer Decision = "defer"
)

// Kind selects which Handler runs a given Config.
type Kind string

const (
	KindCommand Kind = "command"
	KindHTTP    Kind = "http"
	KindPrompt  Kind = "prompt"
)

// Config is one configured hook. Matcher and If together decide whether a
// hook fires for a given call; Kind decides which Handler runs it.
type Config struct {
	Name    string
	Event   Event
	Kind    Kind
	Matcher string // "*", a bare namespace ("platform"), a "namespace/*" glob, or an exact ToolRef string
	If      *Expr  // nil matches unconditionally
	Timeout time.Duration

	// UpdatablePaths is the top-level JSON-field allowlist a rewrite from
	// this hook may touch (task 3.11's "path allowlist that re-binds the
	// digest"). A rewrite touching any other field, or any rewrite at all
	// when this is empty, is refused rather than silently dropped.
	UpdatablePaths []string

	Command string // KindCommand
	Args    []string

	URL string // KindHTTP

	PromptDecision Decision // KindPrompt: fixed decision when this hook matches (defaults to Ask)
	PromptTemplate string   // KindPrompt: text/template source rendered against Context's fields, carried as Outcome.Reason
}

// Context is what one hook dispatch evaluates against. Input is mutated
// in-place across a chain: a hook that validly rewrites it hands the next
// hook (and the eventual re-bound digest) the updated value, never the
// original.
type Context struct {
	ToolID        string
	Namespace     string
	EffectClass   string
	AutonomyLevel string
	DataLabel     string
	Input         json.RawMessage
}

// fields projects Context onto the flat string map Expr.Eval and a prompt
// hook's template read from — a closed, small vocabulary on purpose (the
// same "closed JSON AST, not a string language" discipline README.md §4's
// permission chain and §3 pattern 52 both name).
func (c Context) fields() map[string]string {
	return map[string]string{
		"tool_id":        c.ToolID,
		"namespace":      c.Namespace,
		"effect_class":   c.EffectClass,
		"autonomy_level": c.AutonomyLevel,
		"data_label":     c.DataLabel,
	}
}

// Outcome is one hook's result, or the chain's aggregate.
type Outcome struct {
	Decision     Decision
	Reason       string
	UpdatedInput json.RawMessage // non-nil only once a rewrite has passed the path allowlist
	HookName     string
}

// Handler runs one hook Config against one Context.
type Handler interface {
	Run(ctx context.Context, cfg Config, hctx Context) (Outcome, error)
}
