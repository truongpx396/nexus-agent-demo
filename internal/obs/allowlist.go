// Package obs implements telemetry as a content-free signal class
// (docs/constitution.md: "Observability captures structure, not content").
// This is a property of the pipeline, not a convention: every attribute
// key is checked against a deny-by-default allowlist before it can reach a
// span or metric, and there is no flag, env var, or debug mode that admits
// a different key. Content is reachable only through the event log, under
// an audited, expiring Content Access Grant (Phase 5/6) — never through
// telemetry.
package obs

// allowedAttrs are the only keys Filter ever lets through. Every one of
// them is structure: identifiers, sizes, durations, digests, typed
// outcomes. None of them is, or can become, conversation content.
var allowedAttrs = map[string]struct{}{
	"session.id":              {},
	"tenant.id":               {},
	"root_session.id":         {},
	"parent_session.id":       {},
	"depth":                   {},
	"tool.id":                 {},
	"tool.effect_class":       {},
	"terminal_reason":         {},
	"model.id":                {},
	"usage.input_uncached":    {},
	"usage.input_cache_read":  {},
	"usage.input_cache_write": {},
	"usage.output":            {},
	"latency_ms":              {},
	"active_ms":               {},
	"seq.start":               {},
	"seq.end":                 {},
	"schema_version":          {},
	"outcome":                 {},
}

// Attrs is a set of span/metric attributes prior to filtering. Keys are
// dotted names; values are always strings — telemetry never carries a
// structured value that could itself smuggle content.
type Attrs map[string]string

// Filter drops every key not on the allowlist. It does not truncate or
// redact an unlisted key's value — it removes the key entirely, because a
// truncated content fragment is still content.
func Filter(in Attrs) Attrs {
	out := make(Attrs, len(in))
	for k, v := range in {
		if _, ok := allowedAttrs[k]; ok {
			out[k] = v
		}
	}
	return out
}

// IsAllowed reports whether a single key may reach a span or metric.
func IsAllowed(key string) bool {
	_, ok := allowedAttrs[key]
	return ok
}
