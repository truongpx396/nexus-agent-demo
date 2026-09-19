# Observability stack: Prometheus, Loki, Grafana, Alertmanager, cAdvisor, Tempo, Pyroscope

`docs/build-phases.md`'s Phase 17. README task 13.12 wired `nexusd`'s own
`/metrics` endpoint and `internal/obs.ComputeGoldenSignals` (task 10.12) —
the golden-signal dashboard's own queries — but left the actual scrape
target, log aggregation, and dashboard unbuilt; `handleMetrics`'s own doc
comment in `cmd/nexusd/main.go` says so directly: *"this is what a scrape
target actually needs."* This is that scrape target: a local Prometheus +
Alertmanager + cAdvisor + Loki + Grafana stack, pre-wired with four
dashboards and five alert rules, that turns the metrics and logs this
codebase already emits into something you can actually look at — and page
on. Two more separate, independently opt-in profiles in the same compose
file add Grafana Tempo (generic distributed tracing) and Grafana Pyroscope
(continuous profiling), both correlated in the same Grafana instance.

## Architecture: metrics, logs, alerts, container resource usage — and, optionally, traces + profiles

```
nexusd --(GET /metrics, Prometheus text exposition)--> prometheus --> grafana
cadvisor --(per-container cpu/mem/net/disk)------------>    |
                                                             v
                                                       alertmanager
nexusd/signerd --(zerolog JSON: file OR docker logs)--> promtail --> loki --> grafana
nexusd --(OTLP/HTTP, opt-in profile "tracing")---------> tempo -------------> grafana
nexusd/signerd --(pprof push, opt-in profile "profiling")--> pyroscope ----> grafana
```

`nexusd`/`signerd` also each expose Go's standard `net/http/pprof` handlers
on their own loopback-scoped listener for on-demand, ad hoc profiling
(`go tool pprof`) — that path never touches this compose file at all, see
"On-demand profiling: pprof" below.

Per-run **LLM tracing** — the per-generation/per-tool call tree, token
usage, cost — already has a home purpose-built for it: Langfuse, wired in
[`docs/local-llm.md`](local-llm.md) through `internal/obs.Tracer`. This
stack's base **"observability"** profile deliberately doesn't duplicate
that: it covers the other pillars — aggregate **metrics** (golden signals
across every run, not one run's own call tree — plus a per-tool/per-model
breakdown), **container resource usage** (CPU/memory/network/disk IO per
container — something application-level `/metrics` structurally can't
see), **logs** (what the process itself is doing — retries, circuit
breaks, hook failures — independent of any one session), and **alerts**
(Prometheus evaluating those same golden-signal metrics against a
threshold and routing through Alertmanager, so a regression doesn't
require someone to be staring at a dashboard when it happens).

A separate **"tracing"** profile (below, `make tempo-up`) adds Grafana
Tempo — generic OTLP distributed tracing, not LLM-specific like Langfuse,
but sharing the exact same `obs.OTLPExporter` this codebase already has
(`NEXUS_OTLP_ENDPOINT`, no Langfuse-specific headers or URL path needed).
The point of adding it here rather than reaching for Langfuse every time:
it's a single container with local disk storage, and it lands in the SAME
Grafana this stack already runs, one click away from the golden-signal
metrics and logs a regression usually needs correlated together — where
Langfuse is the right tool for looking at one run's own LLM call tree in
depth, Tempo is the right tool for "what else was slow/erroring around the
same time," without spinning up a second Grafana or a 2-to-6-container
Langfuse stack just to look at spans.

A third, also separately opt-in **"profiling"** profile (below, `make
profiling-up`) adds Grafana Pyroscope — continuous CPU/memory/goroutine/
mutex/block profiling, pushed straight from `nexusd`'s/`signerd`'s own
process via `github.com/grafana/pyroscope-go` (`internal/obs/pyroscope.go`),
not scraped. Where the golden-signal dashboard tells you a regression
happened and Tempo/Langfuse tell you which run was slow, Pyroscope answers
the next question down: which function. It needs no host-network
reachability trick the way Prometheus's `/metrics` target does (see the
Caveats below) — `nexusd`/`signerd` dial OUT to Pyroscope's published port,
so it works identically whether they run as host processes (`make run`) or
containers (`make docker-up`). See "Continuous profiling: Grafana
Pyroscope" below for the full picture, including the on-demand
`net/http/pprof` path this stack doesn't touch at all.

**Recommended: run nexusd/signerd containerized (`make docker-up`), not as
host processes (`make run`)**, when this stack is up — cAdvisor can only
report resource usage for containers it can see, so a host process (the
`make run` default the rest of this codebase's docs use) never shows up in
the Infrastructure dashboard at all, even though its logs and `/metrics`
still work fine either way:

- **Prometheus** reaches `/metrics` over the host network
  (`host.docker.internal:8080`, `deploy/observability/prometheus.yml`)
  regardless of which path you use — `docker-compose.yml`'s `--profile app`
  publishes the same port 8080 to the host, so this one target line never
  needs to change between the two.
- **cAdvisor** only sees containers — nexusd/signerd need `make docker-up`
  to appear on the Infrastructure dashboard at all; postgres/pgbouncer/redis
  show up either way, since those are already containers.
- **Promtail** ships logs two ways, both always on: tailing
  `.dev/nexusd.log`/`.dev/signerd.log` (the `make run` path) and Docker
  service discovery over the socket (the `make docker-up` path) — whichever
  one actually has something to ship just works, no config to flip.

## What you'll see in Grafana

Four dashboards, provisioned automatically (no manual datasource/dashboard
setup — `deploy/observability/grafana/provisioning/`), all under the
**Nexus** folder:

- **Nexus / Golden Signals** — every field of `obs.GoldenSignals` (task
  10.12): completion rate by terminal reason, stuck rate, cost-ceiling
  breach rate, cache-read rate, approval p50/p95 decision latency, approval
  mismatch rate, unresolved in-flight claims, telemetry attribute drop rate.
  A `$tenant_id` template variable filters every panel. Content-free by
  construction: every one of these numbers already went through
  `internal/obs/allowlist.go`'s deny-by-default filter (or, for the ones
  computed straight from Postgres by `ComputeGoldenSignals`, never touched a
  payload column in the first place) before it reached `/metrics` — this
  dashboard cannot show you what a run actually said.
- **Nexus / Cost and Tools** — the per-tool and per-model breakdown the
  Golden Signals board's own tenant-wide aggregates don't show: tool call
  volume by `tool_id` (from `events.tool_id`, a plain structural column —
  never the encrypted payload), and per-model input tokens (split by cache
  class), output tokens, and reconciled cost (from `cost_records`, the same
  reconciled truth `internal/cost.Reconcile` writes, README pattern #37).
  `$tenant_id`-filterable, same content-free posture as every other panel in
  this stack.
- **Nexus / Infrastructure** — per-container CPU (cores), memory (working
  set), network IO, and disk IO. cAdvisor reads this straight from
  containerd (not the classic Docker overlay2 handler, which cannot see a
  container at all under Docker's newer containerd-snapshotter storage
  backend — see Caveats), joined at query time against
  `docker-label-exporter`'s own id→name mapping for real container names,
  since containerd's own metadata doesn't carry Docker's naming. The join
  is also what scopes this dashboard to this project — the exporter is
  server-side filtered by the Docker Engine API itself, so a container it
  was never told about has no `id` to join against and simply isn't in the
  result. `$container`-filterable. Needs `make docker-up`, not `make run`,
  for nexusd/signerd specifically to appear here.
- **Nexus / Logs** — `nexusd`'s/`signerd`'s own structured zerolog output:
  log volume by level (the fastest way to spot a regression before it shows
  up in the slower, session-completion-derived golden signals), a
  pre-filtered error/warn stream, and an unfiltered stream. `$service` and
  `$level` template variables narrow it. Same content-free posture as the
  golden-signal board — these are process logs, never event-log payloads
  (constitution Principle VI); a run's actual input/output never appears
  here, same as it never appears on a span by default (see the Tempo bullet
  below for the one opt-in exception this stack DOES have).

**Tempo** (the separate "tracing" profile) has no dashboard of its own —
Grafana's built-in **Explore** view, pointed at the **Tempo** datasource,
is already a full trace browser (search by service/duration, click into a
trace to see its span tree) with no extra JSON to maintain. Same
allowlist-filtered structure as every span this codebase emits
(`internal/obs/allowlist.go`) — session.id/tenant.id/tool.id/model.id/
usage.\*/outcome/terminal_reason, never conversation content — UNLESS
`NEXUS_TRACE_CONTENT=true` (docs/local-llm.md), which applies identically
here: it's the same `obs.OTLPExporter`/`Span.SetContent` opt-in channel
Langfuse's OTLP path already has, not something specific to Tempo.

## Continuous profiling: Grafana Pyroscope

`internal/obs/pyroscope.go` wraps `github.com/grafana/pyroscope-go` —
`nexusd` and `signerd` each PUSH a continuous stream of CPU, heap
(alloc/inuse, objects/bytes), and goroutine-count profiles to a Pyroscope
server every `UploadRate` (the SDK's own default, 15s), tagged with
`git_commit` (`internal/version.GitCommit`) so a specific build's profile
is identifiable. `NEXUS_PYROSCOPE_ADDR` unset (the default) means
`obs.StartPyroscope` returns immediately and nothing is dialed — the same
zero-setup posture `NEXUS_OTLP_ENDPOINT` unset already has for tracing.

**Mutex/block profiling is a second, separate opt-in** on top of the
server address: `NEXUS_PPROF_MUTEX_FRACTION`/`NEXUS_PPROF_BLOCK_RATE`
(both `0` = off by default) call `runtime.SetMutexProfileFraction`/
`runtime.SetBlockProfileRate` once at startup
(`obs.EnableMutexBlockProfiling`) — confirmed against
`github.com/grafana/pyroscope-go`'s own `session.go`: the SDK never calls
either runtime setter itself, it only reads whatever profile
`runtime/pprof` already has queued, so requesting the `mutex_count`/
`block_count` profile types without also setting a nonzero rate here
uploads an empty profile every interval rather than failing loudly. One
rate setting benefits BOTH consumers on this page at once, since pprof's
own `/debug/pprof/mutex`/`/debug/pprof/block` read the exact same
underlying runtime profile.

Profiles are **process-wide, not tenant-scoped or run-scoped** — one
profile covers every tenant's work interleaved on that process in that
window, the same "process resource usage" pillar cAdvisor's own container
metrics occupy in the architecture diagram above, not the "per-run
behavior" pillar Tempo/Langfuse spans occupy. Correlate a specific tenant's
slow run against a flame graph by timestamp overlap with that run's own
trace, not a shared label. It's also content-free by construction with no
allowlist filtering needed (unlike a span's attributes, `internal/obs/
allowlist.go`) — a profile is function names and call-stack sample counts,
never a run's actual input/output, so there is no `NEXUS_TRACE_CONTENT`
equivalent here and none is needed.

Bring the server up with `make profiling-up` (a THIRD separately opt-in
compose profile, "profiling" — independent of "observability" and
"tracing"; bringing up either of the others never pulls this in). Grafana's
**Explore** view, **Pyroscope** datasource, is a full flame-graph browser
(query by app name — `nexusd` or `signerd` — and profile type) with no
extra dashboard JSON to maintain, the same story Tempo's own Caveats-free
Explore integration already has. Confirmed end-to-end against a live
`grafana/pyroscope:2.3.1` + `grafana/grafana:11.4.0`: the bundled
`grafana-pyroscope-datasource` needs no plugin install on this pinned
Grafana version, its health check reports OK, and a live query against a
pushed `nexusd`-tagged profile returns real flame-graph data.

### On-demand profiling: pprof

Separately from continuous profiling, `obs.StartPprofServer`
(`internal/obs/pprof.go`) exposes Go's standard `net/http/pprof` handlers
(`/debug/pprof/profile`, `/heap`, `/goroutine`, `/trace`, …) for ad hoc
`go tool pprof http://<addr>/debug/pprof/...` or `curl` capture — the
"I need one flame graph right now, from one specific process" tool,
distinct from Pyroscope's "always-on history across every process"
continuous story above. It listens on `NEXUS_PPROF_ADDR` (`nexusd`) /
`NEXUS_SIGNERD_PPROF_ADDR` (`signerd`) — `make run`/`make signerd` set both
unconditionally to `127.0.0.1:6060`/`127.0.0.1:6061` (the same "zero extra
steps for the common path" call already made for `NEXUS_LOG_FILE`), so
`make pprof-cpu`/`pprof-heap`/`pprof-goroutine` work right after a plain
`make run` with nothing to configure; unset either var in your own shell
first if you want that path's listener off.

**`make docker-up` works identically** — `docker-compose.yml`'s own
`nexusd`/`signerd` services set the SAME two env vars, just bound to
`0.0.0.0:6060`/`0.0.0.0:6061` INSIDE the container (a loopback bind there
would be loopback to the container, unreachable through Docker's own port
NAT no matter what's published) with the `ports:` mapping itself doing the
loopback scoping instead — `"127.0.0.1:6060:6060"`, not a bare
`"6060:6060"` — so the actual exposure from the host's own network stack is
identical either way: loopback-only, nothing off the machine can reach it.
`make pprof-cpu`/`pprof-heap`/`pprof-goroutine` need no `PPROF_ADDR`
override for this path — same host-side port either way. (Confirmed this
wasn't automatic: `docker compose up` doesn't rebuild an image that already
exists unless asked, so a plain `docker build -t nexus-agent-demo/nexusd:
latest .` — what `make docker-build` used to run — tagged a DIFFERENT image
name than compose's own auto-generated one and was silently never used;
`make docker-build` now runs `docker compose build` instead, so it always
builds/tags the exact image `docker-up` actually starts.)

And this is the important part, true on both paths — it's **on a SEPARATE
listener from `nexusd`'s main REST/webhook mux and its `/metrics`
endpoint**, never mounted alongside them. `net/http/
pprof`'s own package documentation warns against exposing it on a mux
anything untrusted can reach: `/debug/pprof/profile` lets an unauthenticated
caller pin a CPU core for up to 30s (or longer, caller-settable) with no
auth gate, and `/debug/pprof/heap` dumps live allocation stacks —
meaningfully more sensitive than the "unauthenticated, content-free,
per-process" caveat already documented for `/metrics` below, so this
deliberately doesn't share that endpoint's posture. Loopback is what makes
defaulting it to "on" for `make run` safe in the first place — nothing off
the local machine can reach it; a real deployment wanting it reachable at
all would need the same network-level restriction (or a reverse-proxy auth
gate) the `/metrics` Caveat below already recommends, and doesn't go
through `make run` in the first place (README task 13.1/13.11).

```bash
make pprof-cpu        # 30s CPU profile from nexusd (PPROF_ADDR=127.0.0.1:6060 by default)
make pprof-heap       # in-use heap profile from nexusd
make pprof-goroutine  # goroutine dump from nexusd -- fastest way to spot a leak or a stuck call
PPROF_ADDR=127.0.0.1:6061 make pprof-heap   # same three, against signerd instead
```

Each drops into `go tool pprof`'s own interactive shell (`top`, `web` for an
SVG call graph, `list <func>` for line-level annotation — see `go tool
pprof -h` for the full command set).

## Alerting

Prometheus evaluates `deploy/observability/prometheus-rules.yml` against the
golden-signal gauges above and routes firing alerts to Alertmanager
(`deploy/observability/alertmanager.yml`):

| Alert | Fires when | Severity |
|---|---|---|
| `NexusTargetDown` | `up{job="nexusd"}` is 0 for 1m | critical |
| `NexusStuckRateHigh` | `nexus_stuck_rate` > 0.15 for 5m | warning |
| `NexusCostCeilingBreachRateHigh` | `nexus_cost_ceiling_breach_rate` > 0.3 for 5m | warning |
| `NexusApprovalMismatchDetected` | `nexus_approval_mismatch_rate` > 0 | warning |
| `NexusUnresolvedInFlightClaims` | `nexus_unresolved_inflight_claims` > 0 for 10m | critical |

Every threshold is a starting point for a demo tenant's traffic, not a
calibrated production SLO — tune per tenant once real traffic exists, same
as every other config knob in this plan.

**Alertmanager's `default` receiver has no configured integration** — alerts
still fire, group, and are visible in Alertmanager's own UI/API
(`http://localhost:9094`) and as a Grafana datasource (the **Alertmanager**
entry alongside Prometheus/Loki), but nothing gets sent anywhere. Wiring a
real destination is a `receivers:` block in
`deploy/observability/alertmanager.yml` (Slack/email/webhook/PagerDuty —
Alertmanager's own docs cover the receiver types), the same "config, not a
fork" discipline every other adapter in this plan follows — not a code
change.

## Setup

```bash
# 1. nexusd + signerd — containerized (recommended for this stack, so
#    cAdvisor can see them), or as host processes.
make docker-up          # recommended: containerized, --profile app
# make run               # alternative: host processes (no container resource metrics)

# 2. the observability stack
make observability-up
# grafana:      http://localhost:3310  (admin / nexus-dev-password)
# prometheus:   http://localhost:9091
# alertmanager: http://localhost:9094
# cadvisor:     http://localhost:8095
# loki:         http://localhost:3101
# docker-label-exporter: http://localhost:9101

# 3. optional: Tempo, for generic distributed tracing (a SEPARATE profile —
#    step 2 above never pulls this in on its own)
make tempo-up
# tempo: http://localhost:3200  (OTLP grpc:4417 http:4418)
# then run nexusd/signerd (step 1) with NEXUS_OTLP_ENDPOINT=localhost:4418
# set — no NEXUS_OTLP_HEADERS/NEXUS_OTLP_URL_PATH needed, unlike Langfuse's
# OTLP endpoint (docs/local-llm.md)

# 4. optional: Pyroscope, for continuous profiling (a THIRD SEPARATE
#    profile — steps 2/3 above never pull this in on their own)
make profiling-up
# pyroscope: http://localhost:4040  (own UI, or Grafana's Explore, Pyroscope datasource)
# then run nexusd/signerd (step 1) with NEXUS_PYROSCOPE_ADDR=http://localhost:4040
# set — allow ~60s after Pyroscope's first start before it reports ready
# (see Caveats); uploads simply fail-and-retry on the next tick until then
```

This stack runs as its OWN Compose project, `nexus-agent-observability`
— a separate Docker Desktop container group from the app stack's
`nexus-agent-demo` (postgres/pgbouncer/redis/nexusd/signerd/litellm/
langfuse-lite), the same split `docker-compose.agentic.yml` already made for
Crawl4AI/OpenSandbox with its own `nexus-agent-demo-agentic`. Nothing
functional depends on the two sharing a project: every target this stack
scrapes/tails is reached over a published host port or a bind-mounted file
(see this file's own header comment), never container DNS across that
boundary, and `docker-label-exporter`/Promtail's own Docker-discovery job
both still explicitly target `nexus-agent-demo` regardless of what THIS
file's own project is named — see their own comments in
`docker-compose.observability.yml`/`promtail-config.yml`. `docker compose
ls` shows both projects side by side; `make observability-down` still stops
only this one.

Open Grafana, the **Nexus** folder has all four dashboards waiting. If
Golden Signals shows "No data," confirm Prometheus's own target page
(`http://localhost:9091/targets`) shows the `nexusd` job as `UP` — the most
common cause is `nexusd` not actually listening on `:8080` yet, or running
somewhere `host.docker.internal` can't reach (see Caveats). Prometheus's own
alert-rules page (`http://localhost:9091/alerts`) shows all five rules from
`prometheus-rules.yml`; Alertmanager's UI (`http://localhost:9094`) shows
anything actually firing. If Infrastructure shows "No data," confirm both
the `cadvisor` and `docker-label-exporter` targets are `UP` on that same
targets page — the join between them (see Caveats) needs both.

`make observability-down` stops everything in
`deploy/docker-compose.observability.yml` — the "observability" profile AND
Tempo's "tracing" profile AND Pyroscope's "profiling" profile, whichever
are up — `make down`'s own
postgres/pgbouncer/redis, and `make llm-down`'s/`make agentic-down`'s own
stacks, live in entirely separate compose files, so there's nothing there
for this to touch. `make docker-up`'s own nexusd/signerd containers need
their own `make docker-down` — confirmed that plain `make down` does NOT
stop them (`docker compose down` with no `--profile` flag ignores
profile-gated services that are still running, even though `up` needs the
flag to start them in the first place).

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `NEXUS_LOG_LEVEL` | `info` | `internal/obs.InitLogger`'s floor — any zerolog level name (`trace`/`debug`/`info`/`warn`/`error`). An unparseable value falls back to `info`. |
| `NEXUS_LOG_FILE` | unset (stderr only) | Additionally tee JSON logs to this path, append-only — `make run`/`make signerd` set it to `.dev/nexusd.log`/`.dev/signerd.log` by default; unset it to go back to stderr-only, the behavior before this doc existed. |
| `NEXUS_OTLP_ENDPOINT` | unset (stdout spans) | Set to `localhost:4418` to send spans to Tempo (`make tempo-up`) — the full `NEXUS_OTLP_*`/`NEXUS_LANGFUSE_*` env var set is documented in `docs/local-llm.md`, since this same exporter is shared with the Langfuse tracing path. |
| `NEXUS_PYROSCOPE_ADDR` | unset (profiling disabled) | Set to `http://localhost:4040` to push continuous profiles to Pyroscope (`make profiling-up`) — same value for both `nexusd` and `signerd`; each pushes under its own application name. |
| `NEXUS_PPROF_MUTEX_FRACTION` | `0` (off) | `runtime.SetMutexProfileFraction`'s own parameter — nonzero also adds `mutex_count`/`mutex_duration` to what Pyroscope uploads and makes `nexusd`'s/`signerd`'s own `/debug/pprof/mutex` non-empty. |
| `NEXUS_PPROF_BLOCK_RATE` | `0` (off) | `runtime.SetBlockProfileRate`'s own parameter — nonzero also adds `block_count`/`block_duration` to what Pyroscope uploads and makes `/debug/pprof/block` non-empty. |
| `NEXUS_PPROF_ADDR` | `127.0.0.1:6060` for `make run`; `0.0.0.0:6060` (published loopback-only) for `make docker-up`; unset (no listener) otherwise | `nexusd`'s on-demand `net/http/pprof` listener address — a SEPARATE listener from the main REST/webhook mux, see "On-demand profiling: pprof" above. `make pprof-cpu`/`pprof-heap`/`pprof-goroutine` target `127.0.0.1:6060` by default, working against either path unmodified. |
| `NEXUS_SIGNERD_PPROF_ADDR` | `127.0.0.1:6061` for `make signerd`/`make run`; `0.0.0.0:6061` (published loopback-only) for `make docker-up`; unset otherwise | Same as `NEXUS_PPROF_ADDR`, for `signerd` — a distinct env var since both processes may run on the same host at once and can't share a bind address. |

`docker-label-exporter` (task 17.15) takes two of its own, set in
`docker-compose.observability.yml`'s own service block rather than here —
`COMPOSE_PROJECT` (default `nexus-agent-demo`) is the label value it
filters the Docker API on, and `LISTEN_ADDR` (default `:9101`) is its own
bind address. Neither needs to change for this repo's own use.

## Caveats

- **Every host port this stack publishes is shifted off its tool's own
  default** (Prometheus 9091 not 9090, Alertmanager 9094 not 9093, cAdvisor
  8083 not 8080, Loki 3101 not 3100, Grafana 3310 not 3000) — confirmed
  colliding with an unrelated project's own containers of every one of
  those on the machine this was built on, the same shift-on-confirmed-
  collision call `docker-compose.local-llm.yml` already makes for litellm
  (4000 → 4100) and langfuse-web (3000 → 3001). Container-internal ports
  are unchanged; only the host-side mapping moved. `docker-label-exporter`
  publishes 9101 — its own natural default, no collision found.
- **cAdvisor is pointed at containerd, not dockerd** — on Docker Desktop
  with the containerd image store enabled (`docker info` reporting
  `driver-type: io.containerd.snapshotter.v1`, Docker's now-default
  storage backend, confirmed on the machine this was built on), cAdvisor's
  legacy "docker" container factory cannot register a single container —
  *"failed to identify the read-write layer ID ... no such file or
  directory"*, reproduced against both v0.49.1 and v0.52.1, and confirmed
  it isn't a `-disable_metrics` issue (container registration itself
  aborts). That factory assumes the classic dockerd overlay2 graphdriver
  layout on disk, which doesn't exist under the containerd-snapshotter
  backend. The fix — `docker-compose.observability.yml`'s own `cadvisor`
  service comment has the full derivation — is the same one real
  containerd-based deployments already use (this is exactly how
  kubelet+cAdvisor talk to containerd in any production Kubernetes
  cluster): skip dockerd's storage layer, read straight from containerd's
  own gRPC API (`-containerd`, `-containerd-namespace=moby` — `moby` is
  the namespace dockerd itself uses). No Docker Desktop setting changes,
  no version pin change — still Docker Desktop, still the same cAdvisor
  image, just pointed at the right socket.
- **containerd's "moby" namespace doesn't carry Docker's own container
  name or compose labels** — confirmed directly (`ctr -n moby containers
  info <id>`): the only label present is `com.docker/engine.bundle.path`.
  Docker keeps its naming/labels in dockerd's own metadata store and never
  pushes them into containerd. `docker-label-exporter` (task 17.15) closes
  that gap by reading the same information from the Docker Engine API
  instead — which has always had it correctly, independent of the storage
  backend — and the Infrastructure dashboard joins the two by container ID
  at query time. This is also what scopes the dashboard to this project:
  the exporter's own Docker API call is server-side filtered to
  `com.docker.compose.project=nexus-agent-demo`, so a container it was
  never told about has nothing to join against.
- **cAdvisor runs `privileged: true` with broad read-only host mounts**
  (`/rootfs`, `/sys`, `/var/lib/docker`, the containerd socket) — real,
  broad host access, not something to wave past. This is cAdvisor's own
  documented requirement to collect cgroup data at all (there is no
  narrower flag that still works). `docker-label-exporter` and Promtail's
  Docker-discovery job both need `/var/run/docker.sock` mounted read-only
  for the same class of reason (each needs to ask the Docker daemon a
  question neither cgroups nor containerd can answer).
- **Prometheus's `host.docker.internal` resolution is Docker Desktop's own
  convenience (Mac/Windows); on Linux it needs the `extra_hosts:
  host-gateway` entry `deploy/docker-compose.observability.yml`'s
  `prometheus` service already carries** — confirmed working, but if you're
  running a different container runtime (Podman, etc.) that doesn't honor
  `host-gateway` the same way, point the scrape target at your host's real
  LAN/bridge IP instead.
- **`/metrics` is unauthenticated and per-process, not per-request** — it
  walks every tenant on each scrape (`handleMetrics`'s own `listTenantIDs`
  call). Fine for a local demo's tenant count; a real deployment behind this
  stack would want the same bearer-token gate the rest of the REST surface
  has, or a network-level restriction to the scraper only. Not built here —
  same "deployment concern, not an architectural one" scope line
  `docs/build-phases.md`'s other production-hardening phases already draw.
- **Loki's single-binary mode is a demo topology**, the same call
  `docker-compose.yml` already makes for PgBouncer over a full
  connection-pool mesh — fine for this stack's actual log volume, not the
  microservices deployment Loki's own docs recommend past a few GB/day.
- **Every credential in `deploy/docker-compose.observability.yml` (Grafana's
  admin password) is a fixed, low-entropy dev-only value**, the same spirit
  as this repo's other compose files' own `postgres://nexus:nexus@...` —
  CHANGEME outside a local demo.
- **`nexus_telemetry_attr_drop_rate` always reads 0 from `/metrics`**, not
  because nothing is ever dropped but because `handleMetrics` doesn't thread
  a `DropTracker` through (its own doc comment says so) — the same
  documented limitation `internal/obs.GoldenSignals.TelemetryAttrDropRate`'s
  own doc comment carries. The Golden Signals dashboard's panel for it
  inherits the same caveat rather than hiding the gap.
- **`nexus_tool_call_count` comes from `events.tool_id`, not
  `internal/cost.MeterToolInvocations`** — that meter is registered but
  never emitted (`cost/meter.go`'s own doc comment: "registered but never
  emitted this phase"), so the event log is the only durable source for a
  tool-call count today. It's a raw count, not a rate, and it isn't priced —
  the Cost and Tools dashboard's token/cost panels come from `cost_records`
  instead, a separate query.
- **Alert thresholds in `prometheus-rules.yml` are starting points, not
  calibrated SLOs** — sized for a demo tenant's traffic, not measured
  against real production load. Tune them (and wire a real Alertmanager
  receiver — see Alerting above) before trusting this stack to page anyone.
- **Tempo's local-disk storage backend is a demo topology**, the same call
  Loki's single-binary mode above already makes — fine for a local run's
  actual trace volume, not the object-store-backed deployment Tempo's own
  docs recommend for anything with real retention/throughput needs.
- **Tempo has no span-metrics/service-graph generation wired up** —
  `deploy/observability/tempo.yaml` trims Grafana's own official example's
  `metrics_generator` block on purpose, since enabling it also needs
  Prometheus's `--enable-feature=remote-write-receiver` flag turned on (a
  separate change to the `prometheus` service, not made here). Traces are
  fully queryable via Grafana's Explore either way; what's missing is
  Tempo deriving its own RED metrics/service-graph edges FROM those traces
  into Prometheus.
- **`NEXUS_TRACE_CONTENT=true` applies to whichever OTLP destination is
  configured** — Tempo included, not just Langfuse. Turn it on for local
  debugging; think about whether you want it on for anything real, same
  caveat `docs/local-llm.md` already gives its own OTLP/Langfuse path.
- **Pyroscope's first `/ready` after a cold start (empty volume) takes
  roughly 60 seconds** — confirmed by direct smoke test against
  `grafana/pyroscope:2.3.1`'s own default "all" target with no config
  override: the metastore, ingester, and segment-writer each impose their
  own fixed grace-period timer before reporting ready, one after another,
  not something this compose file's command/config controls. Not a bug on
  either side — `internal/obs.StartPyroscope`'s uploads during that window
  simply fail (logged via the same zerolog sink every other warning in the
  process uses, never fatal) and succeed on the very next `UploadRate` tick
  once the window passes.
- **Pyroscope's own storage backend is local-disk single-binary mode**, the
  same demo-topology call Loki's and Tempo's own Caveats above already make
  — fine for a local run's actual profiling volume, not the object-store-
  backed deployment Pyroscope's own docs recommend for real retention.
- **Mutex/block profiling changes process-wide runtime behavior the moment
  either rate is set**, independent of whether anything ever reads the
  resulting profile — this is Go's own `runtime.SetMutexProfileFraction`/
  `SetBlockProfileRate` cost, not something `internal/obs` adds on top.
  Leave both at `0` (the default) unless actively chasing a contention
  question; a low fraction/rate (this doc's own examples use small values,
  not `1`) keeps sampling overhead bounded the same way Go's own profiling
  guides recommend.
- **`NEXUS_PPROF_ADDR`/`NEXUS_SIGNERD_PPROF_ADDR` have no auth gate of their
  own** — same posture `net/http/pprof`'s own package docs describe, and
  the reason this codebase never mounts them on the mux `/metrics` and the
  REST/webhook surfaces share (see "On-demand profiling: pprof" above).
  `make run`/`make signerd` default them to loopback (`127.0.0.1:...`)
  specifically because that's what makes defaulting them to "on" safe at
  all — nothing off the local machine can reach a loopback bind. Keep it
  that way, or a network only a trusted operator can reach — never the
  public internet, and never carried into a real deployment (which doesn't
  go through `make run` in the first place).
