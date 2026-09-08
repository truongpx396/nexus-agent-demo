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

# 2. Langfuse (optional, but you'll be tracing nothing without it)
make langfuse-up
# first boot takes ~30-60s (clickhouse + minio + migrations) — watch:
#   docker compose -f deploy/docker-compose.local-llm.yml --profile langfuse logs -f langfuse-web
# then open http://localhost:3001 — dev@nexus.local / nexus-dev-password
# (pre-seeded headlessly via LANGFUSE_INIT_* env vars, no signup needed)

# 3. LiteLLM (the gateway nexusd's litellm provider actually talks to)
make llm-up
curl http://localhost:4100/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"qwen2.5-local","messages":[{"role":"user","content":"hi"}]}'

# 4. nexusd, pointed at both
NEXUS_PROVIDER=litellm \
NEXUS_OTLP_ENDPOINT=localhost:3001 \
NEXUS_OTLP_URL_PATH=/api/public/otel/v1/traces \
NEXUS_OTLP_HEADERS='Authorization=Basic cGstbGYtYzBiNWVhMmY2ZmI4OTkwZDFmNTkyMTVlNWY3OWMxNjM6c2stbGYtMWNlMTc2Mjk2Nzk5MGFhY2JmZjU3Yzg5NGNlYTZlMzk=' \
make run
```

`make llm-down` stops everything in `deploy/docker-compose.local-llm.yml` —
`make down`'s own postgres/pgbouncer/redis live in a separate compose file
(`deploy/docker-compose.yml`) entirely, so there's nothing there for this to
touch.

The `Authorization` header above is the fixed dev keypair
`LANGFUSE_INIT_PROJECT_PUBLIC_KEY`/`_SECRET_KEY` already baked into
`deploy/docker-compose.local-llm.yml`'s `langfuse-worker`/`langfuse-web` services,
base64-encoded (`echo -n 'pk-lf-...:sk-lf-...' | base64`). If you change
those keys, regenerate the header the same way — Langfuse's own OTLP docs
confirm this is Basic Auth over `publicKey:secretKey`.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `NEXUS_PROVIDER=litellm` | `fake` | Route model calls through `internal/provider/litellm` instead of the fake/Anthropic adapters |
| `NEXUS_LITELLM_BASE_URL` | `http://localhost:4100` | LiteLLM proxy address (host port 4100, not LiteLLM's own default 4000 — see Caveats) |
| `NEXUS_LITELLM_MODEL` | `qwen2.5-local` | Must match `deploy/litellm/config.yaml`'s `model_name` |
| `NEXUS_LITELLM_API_KEY` | unset | Optional; a local unauthenticated proxy needs none |
| `NEXUS_OTLP_ENDPOINT` | unset (stdout spans) | `host:port` of any OTLP/HTTP collector — Langfuse's self-hosted `langfuse-web`, or any other |
| `NEXUS_OTLP_URL_PATH` | OTel default (`/v1/traces`) | Langfuse's OTLP ingestion path is `/api/public/otel/v1/traces`, not the default |
| `NEXUS_OTLP_HEADERS` | unset | `Key1=Val1,Key2=Val2` — Langfuse needs `Authorization=Basic <base64(publicKey:secretKey)>` |
| `NEXUS_OTLP_INSECURE` | `true` | Disables TLS to the collector — fine over a private/local network |
| `NEXUS_TRACE_CONTENT` | `false` | Opt-in: attach actual prompt/completion/tool input-output to spans (see above) |

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
