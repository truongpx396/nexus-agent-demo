package hooks

import (
	"bytes"
	"context"
	"text/template"
)

// PromptHandler is a declarative hook kind: no script, no endpoint — just a
// matcher/if_expr, a fixed Decision, and a message template rendered against
// the call's fields (tool_id, namespace, effect_class, autonomy_level,
// data_label) and carried as Outcome.Reason. It exists for the common case
// of "always ask (or deny) with this human-readable message when X", which
// would otherwise need a throwaway script for every tenant policy.
type PromptHandler struct{}

func (PromptHandler) Run(_ context.Context, cfg Config, hctx Context) (Outcome, error) {
	decision := cfg.PromptDecision
	if decision == "" {
		decision = Ask
	}

	reason := cfg.PromptTemplate
	if tmpl, err := template.New(cfg.Name).Parse(cfg.PromptTemplate); err == nil {
		var buf bytes.Buffer
		if execErr := tmpl.Execute(&buf, hctx.fields()); execErr == nil {
			reason = buf.String()
		}
	}
	return Outcome{Decision: decision, Reason: reason}, nil
}
