# Production readiness review

**Subject**: `nexus-agent-demo` @ `ccde613` (main, clean tree)
**Scope**: 328 Go files · 45,377 lines · go 1.25.4
**Date**: 2026-09-04
**Method**: `go build ./...`, `go vet ./...`, full `go test -race -cover ./...`, and targeted reads. Every
finding below cites the file and line it was read from.

---

## Verdict

Excellent architecture, unshippable operations, and a model layer that doesn't do what the docs claim.

This is one of the better-reasoned agent codebases I've read — the event log, permission chain, and tenant
isolation are the real thing. But it cannot be deployed, cannot be authenticated, and the single Anthropic
adapter silently undoes three of the patterns README §3 rates at full fidelity.

| Measured | |
|---|---|
| Test files in `kernel/` | **0** |
| Occurrences of `cache_control` repo-wide | **0** |
| Dockerfiles / health endpoints | **0** |
| Eval cases that call a model | **0** of 13 |
| Packages at 0.0% coverage | **7** |
| TODOs in the entire tree | **1** |

Build clean, `go vet` clean, all 34 test packages pass. The problems are not sloppiness — they're scope
boundaries drawn honestly for a demo and never redrawn for production.

---

## A. Ships-blocking (F1–F3)

Nothing here is subtle. Each one independently prevents the binary from being exposed to real traffic.

### F1 — Tenant identity is an unverified request header, so RLS protects against bugs, not attackers

The calling principal is read straight off the wire and parsed as a UUID. That parse is the only validation —
any client that sets a header becomes any tenant. Everything downstream is correct and therefore moot: the
transaction-local `set_config`, the `nexus_app` non-superuser role, and the PgBouncer isolation test all
defend a boundary the front door hands away.

```
internal/surfaces/rest/server.go:144 · capability.go:35 · web/src/lib/api.ts:28

tenantID, err := uuid.Parse(r.Header.Get("X-Nexus-Tenant-ID"))
userID,   err := uuid.Parse(r.Header.Get("X-Nexus-User-ID"))
```

The package doc calls this "a dev stand-in… documented scope, not an oversight," and README §3's diagram
promises `AuthN: static JWT / dev issuer`. There is no JWT anywhere in the repository. Meanwhile the pattern
map rates #49 (surfaces, per-turn principal) at **F — full fidelity**. It isn't; it's the one surface concern
never built.

**Fix** — One verification middleware ahead of the mux, principal from verified claims only. Per-tenant OIDC
is the target; a signed-JWT dev issuer with a rotating key is a day's work and makes every other control in
the trust surface mean something. Delete the header path entirely — leaving it behind a flag is how it
reaches production.

### F2 — `nexusd` has no server lifecycle: no timeouts, no shutdown, no health endpoint

The process ends on a bare `ListenAndServe` whose own suppression comment defers the problem. Because
`serve()` never returns, every `defer` above it is dead code — the queue workers, cron scheduler,
audit-anchor loop, and team backstop are all "stopped" by deferred calls that never run. SIGTERM tears down
in-flight runs mid-turn.

```
cmd/nexusd/main.go:336 — and what a repo-wide grep does not find

return http.ListenAndServe(addr, mux) //nolint:gosec // dev/demo server; timeouts
                                      // are a hardening task, not a Phase 2 one

signal.NotifyContext   →  1 hit, cmd/signerd/main.go:58 only
ReadHeaderTimeout      →  0 hits        /healthz, /readyz  →  0 hits
srv.Shutdown(          →  0 hits        http.Server{       →  0 hits
```

No read/write/idle timeouts means slowloris and unbounded connection growth. No health endpoint means nothing
for a load balancer or Kubernetes probe to attach to — you cannot roll a deploy without dropping requests.

**Fix** — An `http.Server` with the four timeouts, `signal.NotifyContext` driving `Shutdown(ctx)`, and
`/healthz` (process alive) split from `/readyz` (Postgres + Redis + signerd socket reachable). Half a day,
and it unblocks F3.

### F3 — There is no way to package or deploy the binary

`deploy/` contains exactly one file — `docker-compose.yml` for Postgres, PgBouncer, and Redis. That's the
infrastructure the app talks to, not the app. No Dockerfile anywhere in the tree, no image build in CI, no
manifests, no chart. `make build` produces host binaries in `./bin`.

The single-binary thesis is sound and well-argued in README §2 — but "splitting into processes later is a
`main.go` change" only holds if there is a first process to split.

**Fix** — A multi-stage distroless Dockerfile (nonroot, static build; migrations are already embedded via
`migrations/embed.go` so the image is self-contained), a second stage for `signerd`, and a compose profile
running all three against the existing infra services. Manifests can wait; the image cannot.

---

## B. Model-layer defects (F4–F8)

The section to act on first after A, and the one a reader of the README would never suspect. The harness
above the provider port is excellent. The single real adapter beneath it quietly invalidates several of the
properties the harness is built to guarantee.

### F4 — Prompt caching is architected, metered, and gated on, but never actually requested

`cache_control` does not appear anywhere in the repository. The request struct has a plain `System string`
and no breakpoint field, so the API is never told to cache anything. Against the live endpoint,
`cache_read_input_tokens` is 0 on every call, permanently.

```
internal/provider/anthropic/anthropic.go:69 — the whole request shape

type messagesRequest struct {
    Model     string        `json:"model"`
    MaxTokens int           `json:"max_tokens"`
    System    string        `json:"system,omitempty"`   // ← no cache_control, anywhere
    Messages  []anthMessage `json:"messages"`
    Tools     []anthTool    `json:"tools,omitempty"`
    Stream    bool          `json:"stream"`
}

$ grep -rn "cache_control\|CacheControl\|ephemeral" --include="*.go" .
(no matches)
```

Everything around it is real: `promptctx.PrefixBytes` builds a canonical stable zone with a byte-equality
test across turns; `internal/cost` meters `MeterInputCacheRead` and `MeterInputCacheWrite` as first-class
token classes; `docs/go-live.md` item 8 gates launch on ">90% cache-read steady-state." That gate can never
pass. Principle III — "cache-stable context is architecture" — is the one constitutional principle with no
wire-level implementation, and it's also the one carrying the cost argument.

**Fix** — `System` becomes `[]{type,text,cache_control}` with an ephemeral breakpoint closing the stable
zone. Render order is tools → system → messages, and the tool array is already deterministically sorted, so
one breakpoint after `system` covers both. Then assert `InputCacheRead > 0` on turn two of an integration
test — the metering plumbing to check it already exists.

### F5 — Tool calls are flattened into prose at the provider boundary, erasing the invariant the kernel property-tests

A tool result is re-roled to `user` and string-prefixed. Worse, `anthMessage.Content` is a plain `string` —
an assistant turn carrying a `tool_use` block has nowhere to live, so it is dropped from every replayed
history. The model never sees which call produced which result.

```
internal/provider/anthropic/anthropic.go:90 · provider.go:70

if role == "tool" {
    role = "user"
    text = "[tool_result] " + text     // no tool_use_id, no content block
}

type Message struct { Role string; Text string }   // no block structure at all
```

`tests/property/paired_result_test.go` proves the paired `tool_use`/`tool_result` invariant over generated
histories — pattern #4, rated **F** — one layer above the code that discards the pairing. And
`provider.Provider`'s own doc comment reads "Native tool-calling only — no parsing tools out of free-form
text," while the sole real adapter does exactly that on the return path. Expect materially degraded tool
selection on any multi-step task. This also makes F7 unfixable until resolved.

**Fix** — Widen `provider.Message` to carry typed content blocks with `tool_use_id`. This is the one change
here that touches the port rather than the adapter, so do it before the fake provider's scripts calcify
around the flat shape.

### F6 — The release gate never calls a model, so it cannot detect the regressions it exists to catch

All 13 cases (10 visible, 3 held out) are `ProviderScriptCase` values replayed through
`internal/provider/fake`. They test how the harness handles scripted streams — truncation, malformed frames,
throttle refusal, multi-chunk concatenation. Those are good tests. They are not evals.

```
evals/corpus/ — the complete visible corpus

capability_multi_tool_use             negative_malformed_stream_is_an_error
capability_tool_use_with_empty_input  negative_throttle_is_an_error
capability_reasoning_not_leaked...    negative_truncated_mid_tool_use...
regression_basic_stream               safety_truncated_stream_is_an_error
regression_multi_chunk_content...     regression_usage_accounting_stable
```

The statistical machinery around the corpus is genuinely strong — k-trial Wilson intervals, a three-valued
verdict where `inconclusive` never resolves to pass, held-out gap measurement, efficiency banding, baseline
regression. It's aimed at nothing. README §4 says any prompt/tool/model/skill/plan change must clear this
gate; none of those changes can move a single case, because no case's outcome depends on a model.

**Fix** — Ten to fifteen task-completion cases against the live API behind a `-tags=liveeval` guard, graded
by the code graders that already exist. Keep the scripted cases — they're the harness suite. The judge,
trajectory grading, and efficiency bands all become real the moment there's a model behind them.

### F7 — Thinking blocks are billed and discarded; a policy refusal reads as a clean completion

The SSE decoder handles `text_delta` and `input_json_delta` and nothing else — `thinking_delta` and
`signature_delta` fall through the switch silently. The router selects `claude-opus-5` and `claude-sonnet-5`,
both of which run adaptive thinking by default, so those tokens are paid for on every call and dropped on the
floor. They also can't be echoed back on the next turn, which is required when continuing on the same model.

```
internal/provider/anthropic/anthropic.go:317

func stopReasonToDone(reason string) provider.DoneReason {
    case "max_tokens": return provider.DoneMaxOutput
    case "":           return provider.DoneStop
    default:           return provider.DoneStop   // ← "refusal" lands here
}
```

`stop_reason: "refusal"` maps to `DoneStop`. A safety decline is indistinguishable from a successful turn
with empty content — precisely the "typed response classification" failure mode `kernel/classify.go` exists
to prevent, introduced below the layer that classifies. The eval corpus even contains
`capability_reasoning_not_leaked_into_output`, so reasoning was on the radar.

**Fix** — Send `thinking: {type:"adaptive"}` and surface `output_config.effort` as a routing output alongside
model choice (the router already persists its decision and reason, so effort belongs in the same record). Map
`refusal` to a distinct `DoneReason` and carry `stop_details.category`; a refusal deserves its own terminal
reason, not a ninth one hidden inside `DoneStop`. Blocked on F5.

### F8 — Every routed run is mispriced: one wildcard price stands in for three model tiers

The seed writes a single wildcard entry per meter at Sonnet-class figures, while the router chooses between
three models whose real rates differ by 5×.

```
cmd/nexusd/main.go:1886 — seeded, versus what router.go:42 actually selects

seeded (WildcardSubject):  $3.00 in  /  $15.00 out   per 1M

routed  claude-haiku-4-5:   $1.00 in  /   $5.00 out    (3.0× / 3.0× over)
routed  claude-sonnet-5:    $2.00 in  /  $10.00 out    (1.5× / 1.5× over)
routed  claude-opus-5:      $5.00 in  /  $25.00 out    (0.6× / 0.6× under)
```

Confidential and restricted data route to Opus and are billed at 60% of true cost; public simple traffic
routes to Haiku and is billed at 3×. Cost ceilings, pre-spend reservations, and every `budget_decision`
record are enforced against those numbers. The exactness work in `internal/cost/money.go` — integer micros,
no float in the path, a guard test walking the package AST for `float64` — is real engineering spent carrying
wrong inputs precisely.

**Fix** — Per-model entries at real rates, plus a distinct cache-read price (currently seeded at $0.30/M
against a wildcard, but it varies by model). The schema already supports per-subject overrides; only the seed
needs to stop using `WildcardSubject`. An hour's work.

---

## C. Reliability, security, and scale (F9–F15)

These don't block a first deploy the way A does, but each is something to close before the system carries
anyone else's data or traffic.

### F9 — The highest-consequence packages have no unit tests at all

`kernel/` contains zero `_test.go` files. The loop, the response classifier, the terminal-reason producers,
rehydration, and history hygiene are covered only by one property test in `tests/property/` and by
Docker-gated integration tests. Six more packages are at 0.0%, and they are not peripheral ones.

```
go test -cover ./... — the zero and near-zero tail

kernel                     0.0%   1,713 LOC   ← the loop itself
internal/teams             0.0%   1,349
internal/delegate          0.0%   1,130
internal/oversight         0.0%   1,095       ← the approval transaction
internal/runctl            0.0%     929
internal/audit             0.0%     692       ← the hash chain
internal/connectors        0.0%     616
internal/surfaces/rest     5.3%           internal/store   12.2%
internal/obs              12.2%           internal/plan    25.5%
```

That's roughly 6,000 lines of approval transactions, delegation scope descent, and audit chaining with no
test that runs without Docker. The README claims specific tests for several — "a test asserts a condensation
cannot answer 'did the payment go out'" (#26), the `granted_modified` / `approval_mismatch` paths (#23).
Those assertions live in integration tests that CI runs but a developer's `make test` does not.

**Fix** — Table-driven unit tests for `classify.go` and `terminal.go` first: pure functions, high value, an
afternoon. The `exhaustive` linter proves the switch is total; it does not prove any arm is correct.

### F10 — Sandbox containers run as root, with full capabilities, on a writable rootfs

Network default-deny and the CPU/memory/PID ceilings are correctly set. The four settings that actually
resist a container escape are all absent.

```
internal/sandbox/sandbox.go:186 — present versus missing

present:  NetworkMode: "none"        Memory / NanoCPUs / PidsLimit
          Binds: workspace:/workspace   (read-write)

missing:  CapDrop: ["ALL"]           ReadonlyRootfs: true
          SecurityOpt: ["no-new-privileges"]      User: "65534:65534"

$ grep -c "CapDrop\|ReadonlyRootfs\|no-new-privileges\|SecurityOpt" → 0
```

The `isolation` field already carries `gvisor` and `kata` as unshipped values, so the seam for stronger
isolation is there. The Docker baseline underneath it should be hardened regardless — these four lines are
free and they're the difference between a sandbox and a speed bump.

**Fix** — Four fields on `HostConfig` plus a non-root `User`. Mount the workspace read-only where the tool
doesn't need writes.

### F11 — No dependency or supply-chain scanning in CI

The pipeline runs build, lint, unit, integration, eval-gate, and web — a genuinely good set. What it never
does is look at the 60+ transitive dependencies in `go.sum`.

```
.github/workflows/ci.yml — absent jobs

govulncheck   CodeQL   trivy / grype   SBOM   dependabot.yml / renovate.json
                          — none present
```

`gosec` is enabled in `.golangci.yml`, which is SAST over your own code, not your dependencies. For a project
whose entire thesis is a trust surface, an unscanned dependency tree is the conspicuous gap — and the Docker
client, pgx, and OAuth libraries here are all meaningful attack surface.

**Fix** — A `govulncheck ./...` step is three lines and catches the Go-specific case well. Add
`dependabot.yml` for `gomod`, `npm`, and `github-actions`. Image scanning follows F3.

### F12 — Config has no validation, and a missing KEK path silently generates a new key

Configuration is `envOr` calls scattered through `main.go`, each defaulting to a local dev value including
credentials. There's no config struct, no validation, and no fail-fast: a deploy that forgets
`NEXUS_DATABASE_URL` starts cleanly and dials `nexus_app:nexus_app@localhost:6432`. The KEK path is worse.

```
cmd/nexusd/main.go — loadOrGenerateKEK, and the defaults it sits among

f, err := os.Open(path)
if os.IsNotExist(err) {
    kek, _ := crypto.GenerateKEK()      // ← new key, no error, process starts
    os.WriteFile(path, kek.Bytes(), 0o600)
    slog.Info("generated a new dev KEK", "path", path)
}

defaultAppDSN = "postgres://nexus_app:nexus_app@localhost:6432/nexus"
```

A failed volume mount or a typo'd `NEXUS_KEK_PATH` in production produces a healthy-looking process holding a
brand-new KEK — and every existing tenant's DEK becomes unwrappable. The log line says "dev KEK," which is
exactly right in dev and quietly catastrophic anywhere else.

**Fix** — One `Config` struct parsed and validated at startup, erroring on anything unset in a non-dev mode.
Gate KEK generation behind an explicit `--dev` flag: in production a missing KEK file must be a fatal error,
never a generation event.

### F13 — No telemetry leaves the process; the allowlist has nothing behind it

`internal/obs` is a newline-delimited-JSON writer whose own doc calls it "a stand-in for the OTLP exporter
later phases wire up." Those phases shipped; the exporter didn't. The OpenTelemetry packages in `go.mod` are
indirect dependencies pulled in by testcontainers — zero non-test imports.

```
grep -rn "go.opentelemetry" --include="*.go" . | grep -v _test
(no matches)

/metrics        → 0 hits          prometheus  → 0 hits
otlptrace       → 0 hits          otlpmetric  → 0 hits
```

The content-free attribute allowlist is real, deny-by-default, and tested against every content key — that's
the hard half and it's done. But `internal/obs/dashboard.go` prints golden signals to stdout on demand, and
nothing scrapes, alerts, or pages. You cannot operate this system; you can only query it after someone tells
you something is wrong.

**Fix** — An OTLP gRPC exporter behind the existing `Exporter` interface: the filtering seam is already the
right shape, so this is an adapter, not a redesign. Expose the dashboard's golden signals on `/metrics` and
the run-level ones as OTLP spans.

### F14 — Every scale knob is at its zero value, and nothing measures the ceiling

The connection pool is constructed with no config at all, in front of a PgBouncer sized at 20/200. The SSE
reader uses a default `bufio.Scanner`, whose 64KB line cap will hard-error on an oversized frame rather than
degrade. And there is no benchmark or fuzz target in the repository to establish what any of it can take.

```
cmd/nexusd/main.go:176 · anthropic.go:220 · repo-wide

pool, err := pgxpool.New(ctx, dsn)          // MaxConns = max(4, NumCPU)
                                            // no MinConns, MaxConnLifetime,
                                            // HealthCheckPeriod
scanner: bufio.NewScanner(resp.Body)        // no .Buffer() → 64KB line cap

func Benchmark… / func Fuzz…  →  0 across 328 files
```

**Fix** — `pgxpool.ParseConfig` with explicit sizing reconciled against PgBouncer's pool, `scanner.Buffer` at
1MB, and one k6 or vegeta run against `POST /v1/runs` with the fake provider so the queue and pooler have a
measured number rather than an assumed one.

### F15 — The control-plane seam is documented, guarded by a test, and does not exist

README pattern #62 lists `internal/controlplane` as **K** — "Interface + `v1` shapes + import-boundary test."
The package was never created. Two boundary rules name it, and both hit a skip.

```
tests/contract/boundaries_test.go:113 — the escape hatch

if len(pkgs) == 0 || pkgs[0].Name == "" {
    t.Skipf("%s does not exist yet — rule activates once it lands", r.pkg)
}

$ ls internal/controlplane
ls: No such file or directory
```

The skip is deliberate and well-reasoned, and the `checked == 0` backstop at the bottom of the test is a nice
touch. But this specific seam is what makes the whole single-binary collapse honest — README §2 argues the
split "stays a Go interface with a versioned request/response shape, and the packages never import across the
boundary." Two of those three claims are currently a comment. Of 67 catalogued patterns this is the only one
whose stated deliverable is absent, which is a remarkable hit rate — worth closing rather than downgrading.

**Fix** — Either build the package (the interface and `v1` shapes are a day, and the boundary test activates
itself) or downgrade #62 in the map to ✗ with a note. The current state is the only one that misleads.

---

## What's genuinely strong

This has to be said plainly, because the list above is long and the ratio is misleading. Most codebases that
claim this much deliver far less of it.

- **Event sourcing done correctly.** Append-only log, versioned envelope with an upcast registry, and
  projections rebuilt by replay with a test asserting `rebuild == stored`. The discipline that projections
  are never a second source of truth actually holds in the code.
- **The permission chain.** A 10-layer total order, table-driven across layer combinations at 85.8% coverage,
  with an autonomy ratchet that has no widening operation on any interface — the invariant is enforced by the
  type surface, not by a rule someone has to remember.
- **RLS tested the way almost nobody tests it.** Transaction-local `set_config(…, true)` as the only scoping
  call, a separate non-superuser `nexus_app` role so the migration superuser can't mask a policy gap, and the
  isolation test running through PgBouncer in transaction-pooling mode. Testing RLS against a direct
  connection proves nothing, and that's what most projects do.
- **Sign-only key custody.** `signerd` holds the Ed25519 key behind a unix socket, and an import-boundary
  test proves `nexusd` cannot even link the package that reads it. Crypto-shredding erasure destroys the DEK
  while `payload_digest` survives, so the audit chain still verifies after a deletion.
- **The writing.** 45,000 lines with one TODO, and comments that cite the specific requirement each block
  satisfies. When I checked a claim against the code, the code was where the comment said it would be — every
  time except F15.

The through-line in every finding above is the same: **this project consistently built the hard,
expensive-to-retrofit half and deferred the cheap half — and then the cheap half never came.** That's the
correct order, and README §2 argues for it explicitly. The risk now is that the deferred items are invisible
from the documentation, which rates several of them at full fidelity. Fixing F1–F8 is roughly two weeks of
unglamorous work against an architecture that will absorb all of it without a redesign.

---

## Sequence

Ordered by what each gate unlocks, not by severity — F4 is cheaper than F1 but pointless before anything is
deployed.

| Gate | Findings | What it unlocks |
|---|---|---|
| **P0** · ~3 days | F1, F2, F3 | The binary can be **exposed to traffic at all**. Auth makes the trust surface real; lifecycle makes deploys non-destructive; an image makes there be a deploy. |
| **P1** · ~1 week | F5, F4, F8, F7 | The model layer **does what the docs say**. F5 first — it widens the port and F7 depends on it. Cache and pricing next: together they're the difference between a cost model that governs and one that reports fiction. |
| **P2** · ~1 week | F9, F6, F11, F12 | Changes become **safe to make**. Unit tests on the kernel and the approval transaction, evals that can fail for a real reason, a scanned dependency tree, and a config that refuses to start wrong. |
| **P3** · ~1 week | F13, F10, F14, F15 | The system becomes **operable and defensible**. Telemetry that leaves the process, a sandbox that resists escape, measured scale limits, and the last claimed seam either built or retired. |

---

*Severity classes: A = blocker · B = model layer · C = operational.*
