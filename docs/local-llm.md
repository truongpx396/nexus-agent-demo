# Local model + observability: Ollama, LiteLLM, Langfuse

README.md §5 names LiteLLM and Langfuse as third-party integration adapters
this demo defers until "one is actually wanted" — the ports already exist,
and the platform keeps working with all of them off. This is that
integration: a local model (Ollama, running `qwen2.5:3b` natively on the
Mac) reachable through `internal/provider.Provider` via a LiteLLM gateway,
plus genuine per-run tracing in a self-hosted Langfuse.

## Architecture: two independent paths

```
nexusd  --(OpenAI wire protocol)-->  litellm  --(OpenAI wire protocol)-->  ollama (native, :11434)
nexusd  --(OTLP/HTTP, content-free by default)-->  langfuse-web (self-hosted, :3001)
```

**LiteLLM never talks to Langfuse.** It is model-serving infrastructure
only — a pure OpenAI-compatible gateway in front of Ollama
(`deploy/litellm/config.yaml`). Agent observability comes entirely from
nexusd's own `internal/obs.Tracer`, wired into the kernel loop itself
(`kernel/loop.go`), so every surface (REST, CLI, Telegram, cron, …) gets the
same tracing, not just whichever one happens to proxy through an LLM
gateway.

**Ollama runs natively on the Mac**, not in Docker — Metal acceleration
needs the host GPU, which a container doesn't get on Docker Desktop for
Mac. LiteLLM and Langfuse are both containerized, reaching Ollama over
`host.docker.internal:11434`.

**Langfuse isn't the only place `NEXUS_OTLP_ENDPOINT` can point.** Since
`obs.OTLPExporter` speaks plain OTLP, it works against any OTLP/HTTP
collector — `docs/observability.md`'s own opt-in Grafana Tempo profile
(`make tempo-up`) is a second, lighter option, correlated in the same
Grafana the metrics/logs stack already runs, with no Langfuse-specific
headers or URL path to set. Reach for Langfuse when you want the
per-generation/per-tool LLM view (cost, token usage, prompt/completion);
reach for Tempo when you just want "is a trace showing up at all" or want
it next to the golden-signal dashboards. See that doc for setup.

**`NEXUS_OTLP_ENDPOINT` and `NEXUS_LANGFUSE_HOST` can both be set at
once** — `newSpanExporter` (`cmd/nexusd/main.go`) fans out through
`obs.NewMultiExporter` (`internal/obs/multi.go`) rather than requiring a
pick, so e.g. Tempo AND `langfuse-lite` can receive the exact same run's
spans simultaneously, each correctly nested in its own id space (verified:
one run, both backends, matching `session.id` on each). This is genuinely
more than a bare fan-out: `OTLPExporter` and `LangfuseExporter` both
propagate parent/child linkage through the same
`go.opentelemetry.io/otel/trace.SpanContext` mechanism, so `MultiExporter`
gives each of them its own private ctx thread — otherwise whichever ran
second would silently overwrite the other's notion of "current span,"
corrupting nesting specifically for delegated subagent runs. That's also
why `internal/delegate/spawn.go`'s cross-goroutine propagation now goes
through the generic `obs.Tracer.Detach`/`.Attach` seam instead of calling
`go.opentelemetry.io/otel/trace` directly — it has to work no matter which
Tracer (or combination) is actually wired in.

## What you'll see in Langfuse, and why

Every run through the kernel loop produces one Langfuse trace: a root
`agent` span (the whole `Run`/`Resume`/`Continue` call), a `generation`
child per model turn, and a `tool` child nested under the turn that
requested it — a real call tree, not a flat list, because
`internal/obs.Tracer.StartSpan` uses standard OTel parent/child context
propagation (`kernel/loop.go`'s `turnCtx` is what makes a turn's tool spans
nest under that turn's own generation span rather than the root).

**By default, no span carries a prompt, completion, or tool payload.**
`docs/constitution.md` Principle VI is explicit: *"Content is reachable
only through the event log, under an audited, expiring Content Access
Grant — never through telemetry."* `internal/obs/allowlist.go`'s
deny-by-default allowlist is what enforces that for every span this system
emits, tracing included — you'll see `model.id`, token counts, `tool.id`,
`outcome`, `terminal_reason`, timing. You will NOT see what was actually
said or what a tool actually returned. That's the architecture working as
designed, not a gap in this integration.

**Set `NEXUS_TRACE_CONTENT=true`** (`kernel.Kernel.TraceContent`) to see the
actual input/output too — the user's message and the model's response/tool
calls on each generation span, a tool's real arguments and result on each
tool span, via Langfuse's own universal `langfuse.observation.input` /
`.output` attributes (confirmed against Langfuse's OTel ingestion docs as
working for every observation type, not just generations). This is a
**separate, explicitly opt-in channel** (`obs.Span.SetContent`) — it does
not loosen `internal/obs`'s allowlist, which stays exactly as strict as
before; it is a second, independent way content can leave the process,
analogous to (but distinct from) the Content Access Grant Principle VI
already carves out as the sanctioned exception. Turn it on for local
debugging against qwen2.5:3b; think about whether you want it on for
anything that isn't.

## Setup

```bash
# 1. Ollama, natively on the Mac (not Docker)
brew install ollama        # or download from ollama.com
ollama serve &              # or just use the menubar app
make ollama-pull             # pulls qwen2.5:3b (~2GB)

# 2. Langfuse (optional, but you'll be tracing nothing without it) --
#    pick ONE of these two, not both (same host port 3001):

# 2a. Full stack (OTLP, real per-run call tree, heavier: 6 containers)
make langfuse-up
# first boot takes ~30-60s (clickhouse + minio + migrations) — watch:
#   docker compose -f deploy/docker-compose.local-llm.yml --profile langfuse logs -f langfuse-web

# 2b. OR: lightweight stack (native ingestion, 2 containers: web + postgres)
make langfuse-lite-up
# boots in a few seconds — no clickhouse/minio/worker to wait on

# either way: open http://localhost:3001 — dev@nexus.local / nexus-dev-password
# (pre-seeded headlessly via LANGFUSE_INIT_* env vars, no signup needed)

# 3. LiteLLM (the gateway nexusd's litellm provider actually talks to)
make llm-up
curl http://localhost:4100/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"qwen2.5-local","messages":[{"role":"user","content":"hi"}]}'

# 4. nexusd, pointed at LiteLLM plus WHICHEVER Langfuse profile you chose:

# 4a. against the full stack (2a) — OTLP
NEXUS_PROVIDER=litellm \
NEXUS_OTLP_ENDPOINT=localhost:3001 \
NEXUS_OTLP_URL_PATH=/api/public/otel/v1/traces \
NEXUS_OTLP_HEADERS='Authorization=Basic cGstbGYtYzBiNWVhMmY2ZmI4OTkwZDFmNTkyMTVlNWY3OWMxNjM6c2stbGYtMWNlMTc2Mjk2Nzk5MGFhY2JmZjU3Yzg5NGNlYTZlMzk=' \
make run

# 4b. OR: against the lightweight stack (2b) — native ingestion, no OTLP
NEXUS_PROVIDER=litellm \
NEXUS_LANGFUSE_HOST=http://localhost:3001 \
NEXUS_LANGFUSE_PUBLIC_KEY=pk-lf-c0b5ea2f6fb8990d1f59215e5f79c163 \
NEXUS_LANGFUSE_SECRET_KEY=sk-lf-1ce1762967990aacbff57c894cea6e39 \
make run
```

**`make docker-up` (containerized nexusd/signerd) works too**, no inline env
vars needed — `docker-compose.yml`'s own `nexusd` service reads
`NEXUS_PROVIDER`/`NEXUS_LITELLM_MODEL`/`NEXUS_LITELLM_API_KEY` from your
repo-root `.env` (`make`'s `ENV_FILE_FLAG` passes `--env-file .env` so this
resolves correctly), so `NEXUS_PROVIDER=litellm` in `.env` is all `.env`
needs for this path too. `NEXUS_LITELLM_BASE_URL` is hardcoded to
`http://litellm:4000` regardless of what's in `.env` — container DNS +
LiteLLM's own internal port, since `.env`'s own `http://localhost:4100`
(correct for a HOST process reaching LiteLLM's published port) would
resolve to the nexusd container itself from inside it. `docker-compose.yml`
and `docker-compose.local-llm.yml` share one project name/network
specifically so this reaches LiteLLM without any extra wiring — bring
LiteLLM up first (`make llm-up`) or after, order doesn't matter, nexusd just
retries until it's reachable. Langfuse/OTLP tracing isn't wired into this
path yet (`docker-compose.yml`'s own `nexusd` service doesn't pass through
`NEXUS_OTLP_*`/`NEXUS_LANGFUSE_*`) — only the model-serving half of this
doc's Setup applies to `make docker-up` today.

`make llm-down` stops everything in `deploy/docker-compose.local-llm.yml` —
`make down`'s own postgres/pgbouncer/redis live in a separate compose file
(`deploy/docker-compose.yml`) entirely, so there's nothing there for this to
touch.

The `Authorization` header above is the fixed dev keypair
`LANGFUSE_INIT_PROJECT_PUBLIC_KEY`/`_SECRET_KEY` already baked into
`deploy/docker-compose.local-llm.yml`'s `langfuse-worker`/`langfuse-web` services,
base64-encoded (`echo -n 'pk-lf-...:sk-lf-...' | base64`). If you change
those keys, regenerate the header the same way — Langfuse's own OTLP docs
confirm this is Basic Auth over `publicKey:secretKey`. The lightweight
path's `NEXUS_LANGFUSE_PUBLIC_KEY`/`_SECRET_KEY` are the same two values,
unencoded — `internal/obs.LangfuseExporter` sends Basic auth itself, no
header to build by hand.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `NEXUS_PROVIDER=litellm` | `fake` | Route model calls through `internal/provider/litellm` instead of the fake/Anthropic adapters |
| `NEXUS_LITELLM_BASE_URL` | `http://localhost:4100` | LiteLLM proxy address (host port 4100, not LiteLLM's own default 4000 — see Caveats) |
| `NEXUS_LITELLM_MODEL` | `qwen2.5-local` | Must match `deploy/litellm/config.yaml`'s `model_name` |
| `NEXUS_LITELLM_API_KEY` | unset | Optional; a local unauthenticated proxy needs none |
| `NEXUS_OTLP_ENDPOINT` | unset (stdout spans) | `host:port` of any OTLP/HTTP collector — Langfuse's self-hosted `langfuse-web` (full profile), or any other. Mutually exclusive with `NEXUS_LANGFUSE_HOST` below — set only one |
| `NEXUS_OTLP_URL_PATH` | OTel default (`/v1/traces`) | Langfuse's OTLP ingestion path is `/api/public/otel/v1/traces`, not the default |
| `NEXUS_OTLP_HEADERS` | unset | `Key1=Val1,Key2=Val2` — Langfuse needs `Authorization=Basic <base64(publicKey:secretKey)>` |
| `NEXUS_OTLP_INSECURE` | `true` | Disables TLS to the collector — fine over a private/local network |
| `NEXUS_LANGFUSE_HOST` | unset | `http://host:port` of a self-hosted Langfuse — routes through `internal/obs.LangfuseExporter`'s native Ingestion API instead of OTLP, the only path a lightweight Langfuse v2 (`langfuse-lite` profile) understands. Mutually exclusive with `NEXUS_OTLP_ENDPOINT` — both set is a startup error |
| `NEXUS_LANGFUSE_PUBLIC_KEY` / `NEXUS_LANGFUSE_SECRET_KEY` | unset | Basic-auth credentials for `NEXUS_LANGFUSE_HOST`, sent as-is (no manual base64/header) — required together with it |
| `NEXUS_TRACE_CONTENT` | `false` | Opt-in: attach actual prompt/completion/tool input-output to spans (see above) — honored by both the OTLP and native-ingestion exporters identically |

## Caveats

- **qwen2.5:3b's tool-calling through Ollama is noticeably weaker than a
  frontier hosted model.** Fine for exercising the wire protocol and a local
  dev loop; not a production-quality tool-calling backend.
- **LiteLLM's own default port, 4000, is a common local-dev collision** —
  confirmed against a real conflict while building this integration.
  `deploy/docker-compose.local-llm.yml` publishes it on host port 4100 instead (the
  container's own port stays 4000); if 4100 also collides on your machine,
  change the host side of that one port mapping and
  `NEXUS_LITELLM_BASE_URL` together.
- **The self-hosted Langfuse stack is genuinely 6 containers** (web,
  worker, ClickHouse, MinIO, Redis, Postgres) — that's Langfuse v4's only
  supported self-host topology, not something this integration adds
  weight to. `make langfuse-up`/`make llm-down` to bring it up only when
  you're actively looking at traces. It wants real headroom — Langfuse's
  own docs suggest 4 cores / 16GB; on a busy Docker Desktop VM
  (many other containers already running) `langfuse-web` can get
  OOM-killed (`docker inspect <container> --format '{{.State.OOMKilled}}'`
  will say `true`) even though every service and its migrations are
  otherwise configured correctly — free up memory (stop unrelated
  containers, or `make llm-down` anything you're not actively using) and
  retry rather than assuming something's broken.
- **Every secret in `deploy/docker-compose.local-llm.yml`'s langfuse-\* services and
  `NEXUS_TRACE_CONTENT`'s example above is a fixed, low-entropy dev-only
  value**, the same spirit as this file's existing `postgres://nexus:nexus@...`
  — CHANGEME outside a local demo, and think twice before turning
  `NEXUS_TRACE_CONTENT` on anywhere real content would flow.
- **The `langfuse-lite` profile + `NEXUS_LANGFUSE_HOST` speak a protocol
  Langfuse itself has deprecated**: Langfuse's native Ingestion API predates
  OTLP and its own docs mark it deprecated in favor of OTLP (Cloud sunset
  2026-11-16). That date is Langfuse Cloud's own sunset, not a deadline for
  a self-hosted, version-pinned `langfuse/langfuse:2` image — nothing forces
  an upgrade, so it keeps working — but this is new code written against a
  protocol its own vendor is retiring, accepted specifically to keep the
  local trace stack at 2 containers instead of 6. If Langfuse ever pulls the
  `:2` tag or the endpoint stops responding entirely, the fix is `make
  langfuse-up` (the OTLP-compatible full stack), not a patch to this path.
- **The lightweight path has no "agent" observation kind** — Langfuse's
  legacy Ingestion API only has SPAN/GENERATION/EVENT, unlike the OTLP
  path's `langfuse.observation.type=agent` attribute (otlp.go). A run's root
  span and its tool calls both send as plain SPAN observations. This is a
  narrower cosmetic gap than it might sound: the UI still tells them apart
  fine by name + tree position (`kernel.run` vs `tool.call`, nested exactly
  where they happened) — the same way a LangGraph app's Langfuse v2 trace
  reads tool calls and subagents apart, since that integration doesn't rely
  on a dedicated "agent" observation type either. What's actually missing is
  only Langfuse's specific agent-graph *visualization mode*, an OTLP+v3/v4
  feature — the OTLP path (`make langfuse-up`) is the one to reach for if
  that particular view matters more than container count.
- **Delegated subagent runs (`internal/delegate`) DO correctly nest as one
  trace, not a separate one per subagent** — worth calling out explicitly
  because it's easy to get wrong: `internal/delegate/spawn.go` carries a
  delegated child's parent span across its own goroutine boundary via
  `go.opentelemetry.io/otel/trace`'s vendor-neutral `SpanContext`
  propagation (`trace.ContextWithRemoteSpanContext`) — the same mechanism
  real OTel spans use, kept independent of any particular exporter on
  purpose. `LangfuseExporter.StartSpan` participates in that same
  propagation (reads/writes a real `trace.SpanContext`, using its
  TraceID/SpanID as the Langfuse ids it sends) specifically so this works
  without spawn.go needing to know which exporter is active. An earlier
  version of this exporter used a private ctx key instead and silently
  failed this — every delegated subagent opened as a second, disconnected
  trace, correlated only by `session.id` metadata, not by nesting.
  `TestLangfuseExporter_NestsAcrossGoroutineBoundaryLikeDelegateSpawn`
  (`internal/obs/langfuse_test.go`) copies spawn.go's exact propagation
  pattern to guard against that regression.
- **`langfuse-up` and `langfuse-lite-up` are alternatives, never both at
  once** — same host port 3001, and nexusd's two exporter configs
  (`NEXUS_OTLP_*` vs. `NEXUS_LANGFUSE_*`) are mutually exclusive by design
  (`newSpanExporter` errors out at startup if both are set).
- **Confirmed against a real `langfuse-lite` instance** (a `litellm`-backed
  run, `NEXUS_TRACE_CONTENT` tried both on and off): the legacy `usage`
  field on a generation event IS recognized by the v2 server — it correctly
  populates Langfuse's own `promptTokens`/`completionTokens`/`totalTokens`
  and cost UI, so there was no need to also send the newer `usageDetails`.
  And `Emit`'s traceId-less `event-create` (the REST surface's one-off
  point events, e.g. a run's terminal event) is NOT dropped — Langfuse
  auto-creates an implicit single-observation trace for it (its `id` equals
  the trace's own `id`). Both were open questions at implementation time;
  both now confirmed rather than assumed.
