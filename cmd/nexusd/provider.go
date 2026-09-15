package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/anthropic"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/litellm"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
)

// newProvider picks the fake (default, no credentials needed — the demo
// command in README.md §5 must work with zero setup), the real Anthropic
// adapter, or a local model reached through LiteLLM (docs/local-llm.md —
// Ollama running natively on the host, LiteLLM as the OpenAI-compatible
// gateway in front of it), never more than one: correctness tests always
// run against internal/provider/fake regardless of this switch
// (constitution Principle IX) — this only controls what a live `nexusd run`
// talks to.
func newProvider() (provider.Provider, error) {
	switch envOr("NEXUS_PROVIDER", "fake") {
	case "anthropic":
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("NEXUS_PROVIDER=anthropic requires ANTHROPIC_API_KEY")
		}
		model := envOr("NEXUS_ANTHROPIC_MODEL", "claude-sonnet-5")
		return anthropic.New(apiKey, model), nil
	case "litellm":
		baseURL := envOr("NEXUS_LITELLM_BASE_URL", "http://localhost:4100")
		model := envOr("NEXUS_LITELLM_MODEL", "qwen2.5-local")
		apiKey := os.Getenv("NEXUS_LITELLM_API_KEY") // optional; empty is fine for a local, unauthenticated proxy
		return litellm.New(baseURL, model, apiKey), nil
	case "fake":
		// An echo-style script: enough to drive a real turn through the
		// loop without a live model. Real scripted corpora live in
		// evals/corpus/ and internal/provider/fake's own tests; this is
		// just what an unscripted `nexusd run` demo has to say.
		//
		// repeatingFakeProvider, not a bare fake.New(...): fake.Provider
		// pops one script per Stream() call and hard-errors once its
		// (here, one-element) list is exhausted — the correct, intentional
		// contract for a test that constructs its own scripted sequence and
		// wants a call past the end of it to fail loudly. But this one
		// Provider is shared for the whole process's lifetime, across every
		// session `nexusd serve`/`--dev` ever handles — without repeating,
		// only the very first Stream() call this process ever makes (across
		// ALL tenants/sessions) succeeds; every message after that, forever,
		// fails with "no script left for call #2". Wrapping it so every
		// call gets its own fresh one-script Provider is what makes
		// NEXUS_PROVIDER=fake an honest zero-setup interactive default
		// rather than a single-shot demo that silently breaks after one
		// reply.
		return repeatingFakeProvider{script: fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "content", Text: "Hello from the Phase 2 kernel loop demo."},
			{Kind: "usage", InputUncached: 120, OutputTokens: 18},
			{Kind: "done", Done: "stop"},
		}}}, nil
	default:
		return nil, fmt.Errorf("unknown NEXUS_PROVIDER %q (want fake, anthropic, or litellm)", os.Getenv("NEXUS_PROVIDER"))
	}
}

// repeatingFakeProvider adapts fake.Provider (a finite, once-through scripted
// sequence — the right contract for a test that owns its own script list)
// into a Provider that never runs out for a long-lived interactive process:
// each Stream call gets a brand-new single-script fake.Provider, so the
// SAME canned reply is available for every turn of every session, not just
// the process's first-ever call. See newProvider's "fake" case for why this
// exists rather than sharing one fake.New(...) instance.
type repeatingFakeProvider struct {
	script fake.Script
}

func (r repeatingFakeProvider) Stream(ctx context.Context, p provider.Prompt, tools []provider.ToolSchema, rc provider.RunContext) (provider.Stream, error) {
	return fake.New(r.script).Stream(ctx, p, tools, rc)
}

// spanExporter is what newSpanExporter returns: rest.Server.Exporter's own
// point-event Emit, plus obs.Tracer's real start/end spans that
// kernel.Kernel.Tracer uses (docs/local-llm.md) — obs.Exporter and
// obs.OTLPExporter both satisfy this already, this interface just names the
// combination once instead of at each of the two call sites that need it.
type spanExporter interface {
	rest.SpanEmitter
	obs.Tracer
}

// newSpanExporter builds whichever obs Exporter(s) this process emits
// spans through (README task 13.12, closing production-readiness finding
// F13): NEXUS_LANGFUSE_HOST set means the native-ingestion exporter
// (obs.LangfuseExporter, docs/local-llm.md's lightweight self-hosted
// Langfuse v2 path — no OTLP collector needed); NEXUS_OTLP_ENDPOINT set
// means a real OTLP collector (obs.OTLPExporter — Grafana Tempo,
// docs/observability.md, or Langfuse's own OTLP endpoint, the heavier
// v3+/v4 self-hosted path). Unlike earlier, these are NOT mutually
// exclusive — both set means both run, fanned out through
// obs.NewMultiExporter (docs/local-llm.md's own "simultaneous tracing"
// section) so the same run's spans reach both destinations at once, each
// correctly nested in its OWN id space. Neither set falls back to the
// stdout obs.Exporter every earlier phase already had. The filtering
// guarantee (internal/obs's allowlist) is identical across every
// combination, only the sink(s) change. The returned shutdown func flushes
// every exporter actually built; it's a no-op for the stdout one, which
// has nothing to flush.
//
// NEXUS_OTLP_URL_PATH and NEXUS_OTLP_HEADERS exist specifically to reach a
// collector that isn't a bare local OTel collector — Langfuse's OTLP
// ingestion lives at a non-default path and needs a Basic-auth header
// (docs/local-llm.md spells out the exact values for its self-hosted dev
// stack). Both are optional; a plain local collector (Tempo included)
// needs neither.
func newSpanExporter(ctx context.Context) (spanExporter, func(), error) {
	var exporters []spanExporter
	var shutdowns []func()

	if langfuseHost := envOr("NEXUS_LANGFUSE_HOST", ""); langfuseHost != "" {
		publicKey := envOr("NEXUS_LANGFUSE_PUBLIC_KEY", "")
		secretKey := envOr("NEXUS_LANGFUSE_SECRET_KEY", "")
		if publicKey == "" || secretKey == "" {
			return nil, nil, fmt.Errorf("NEXUS_LANGFUSE_HOST set but NEXUS_LANGFUSE_PUBLIC_KEY/NEXUS_LANGFUSE_SECRET_KEY missing")
		}
		exp := obs.NewLangfuseExporter(langfuseHost, publicKey, secretKey)
		exporters = append(exporters, exp)
		shutdowns = append(shutdowns, func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := exp.Shutdown(shutdownCtx); err != nil {
				log.Error().Err(err).Msg("nexusd: shutdown Langfuse span exporter")
			}
		})
	}

	if endpoint := envOr("NEXUS_OTLP_ENDPOINT", ""); endpoint != "" {
		headers := parseHeaderList(envOr("NEXUS_OTLP_HEADERS", ""))
		urlPath := envOr("NEXUS_OTLP_URL_PATH", "")
		exp, err := obs.NewOTLPExporter(ctx, endpoint, envOr("NEXUS_OTLP_INSECURE", "true") == "true", headers, urlPath)
		if err != nil {
			return nil, nil, err
		}
		exporters = append(exporters, exp)
		shutdowns = append(shutdowns, func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := exp.Shutdown(shutdownCtx); err != nil {
				log.Error().Err(err).Msg("nexusd: shutdown OTLP span exporter")
			}
		})
	}

	combinedShutdown := func() {
		for _, s := range shutdowns {
			s()
		}
	}
	switch len(exporters) {
	case 0:
		return obs.NewExporter(os.Stdout), func() {}, nil
	case 1:
		return exporters[0], combinedShutdown, nil
	default:
		// Explicit indices, not exporters... : []spanExporter isn't
		// assignable to obs's own unexported ...target variadic parameter
		// (distinct named interface types, even though structurally
		// identical) — Go's slice-spread assignability rule is stricter
		// than a single value's implicit interface conversion. Only two
		// candidates exist above, so this always covers exactly this case.
		return obs.NewMultiExporter(exporters[0], exporters[1]), combinedShutdown, nil
	}
}

// parseHeaderList parses "Key1=Val1,Key2=Val2" into a map — same shape as
// the comma-split env vars this file already has (NEXUS_WEB_FETCH_ALLOWLIST,
// NEXUS_OAUTH_PROVIDERS), just with a "=" split on each element too. An
// empty raw string returns nil, not an empty-but-non-nil map, so
// NewOTLPExporter's own `len(headers) > 0` check skips WithHeaders entirely
// when nothing was configured.
func parseHeaderList(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	headers := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return headers
}

// newEmbedder returns this demo's one Embedder: internal/provider/fake's
// deterministic fake (README task 12.5 — "no correctness test calls a live
// embedding model"). Unlike newProvider, there is no real-adapter branch
// here: Anthropic has no first-party embedding endpoint, and adding a
// third-party embedding vendor's adapter is a real-integration concern
// README §8's own trigger table gates behind "one is actually wanted" —
// nothing in Phase 12's own task list (12.1-12.8) asks for one. Swapping a
// real embedder in later is exactly the kind of change internal/provider.
// Embedder's port exists to make an internal one: a new adapter package
// behind the same interface, no caller-visible change here beyond this
// function's own body.
func newEmbedder() provider.Embedder {
	return fake.NewEmbedder()
}
