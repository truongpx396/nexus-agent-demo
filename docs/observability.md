# Observability stack: Prometheus, Loki, Grafana, Alertmanager, cAdvisor

`docs/build-phases.md`'s Phase 17. README task 13.12 wired `nexusd`'s own
`/metrics` endpoint and `internal/obs.ComputeGoldenSignals` (task 10.12) —
the golden-signal dashboard's own queries — but left the actual scrape
target, log aggregation, and dashboard unbuilt; `handleMetrics`'s own doc
comment in `cmd/nexusd/main.go` says so directly: *"this is what a scrape
target actually needs."* This is that scrape target: a local Prometheus +
Alertmanager + cAdvisor + Loki + Grafana stack, pre-wired with four
dashboards and five alert rules, that turns the metrics and logs this
codebase already emits into something you can actually look at — and page
on.

## Architecture: metrics, logs, alerts, and container resource usage, not traces

```
nexusd --(GET /metrics, Prometheus text exposition)--> prometheus --> grafana
cadvisor --(per-container cpu/mem/net/disk)------------>    |
                                                             v
                                                       alertmanager
nexusd/signerd --(zerolog JSON: file OR docker logs)--> promtail --> loki --> grafana
```

Per-run **tracing** already has a home — Langfuse, wired in
[`docs/local-llm.md`](local-llm.md) through `internal/obs.Tracer` — and is
deliberately not duplicated here. This stack covers the other observability
pillars: aggregate **metrics** (golden signals across every run, not one
run's own call tree — plus a per-tool/per-model breakdown), **container
resource usage** (CPU/memory/network/disk IO per container — something
application-level `/metrics` structurally can't see), **logs** (what the
process itself is doing — retries, circuit breaks, hook failures —
independent of any one session), and **alerts** (Prometheus evaluating
those same golden-signal metrics against a threshold and routing through
Alertmanager, so a regression doesn't require someone to be staring at a
dashboard when it happens).

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
  here, same as it never appears on a span (`docs/local-llm.md`'s own
  explanation of why `NEXUS_TRACE_CONTENT` is a separate, explicit channel
  applies here too — this stack has no equivalent opt-in).

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
# cadvisor:     http://localhost:8083
# loki:         http://localhost:3101
# docker-label-exporter: http://localhost:9101
```

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
`deploy/docker-compose.observability.yml` — `make down`'s own
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
