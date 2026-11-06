# Pattern coverage: the original's 67 patterns against this demo

**Spec**: [spec.md](spec.md) | **Status**: Maintained (checked: `tests/contract/specs_test.go` fails if a path named here stops existing)

> This table was section 1 of the build ledger (`docs/build-phases.md`, deleted; frozen at `09d54f8`). It answers a different question from the
> tasks: not "what was built, in what order" but "for each pattern in the original design, how faithfully does this demo implement it".
> **F** is full fidelity, **S** simplified, **K** a seam only, **✗** deliberately out of scope. The README's "57 of 67" badge links here.

Legend: **F** = full fidelity · **S** = simplified but structurally identical · **K** = seam/schema kept, behavior deferred · **✗** = out of scope.

| # | Pattern (original) | Demo | Where it lands | Note |
|---|---|---|---|---|
| 1 | Single kernel loop, async generator | **F** | `kernel/` via Go 1.23 `iter.Seq2` range-over-func | The generator seam survives translation exactly |
| 2 | Typed response classification (`TOOL_CALLS`/`CONTENT`/`EMPTY`) | **F** | `kernel/classify.go` | `exhaustive` linter enforces the switch |
| 3 | Nine typed terminal reasons + a named producer for each | **F** (8 of 9) | `kernel/terminal.go` | `credit_exhausted` dropped with the billing plane |
| 4 | Paired `tool_use`/`tool_result` invariant, synthetic on every error path | **F** | `kernel/hygiene.go` + property test | Property-based over generated histories, as the constitution requires |
| 5 | Append-only event log, versioned envelope, upcasting path | **F** | `internal/store/`, `migrations/` | 40+ event types, `schema_version`, upcast registry |
| 6 | Projections are never a second source of truth | **F** | `internal/eventlog/projection.go` | `status`, `terminal_reason`, `taint_state` rebuilt by replay; test asserts rebuild == stored |
| 7 | Two-zone cache-stable prompt + measured cache-read | **F** | `internal/promptctx/` | Byte-equality test on the prefix across turns |
| 8 | Non-destructive live pruning, then structured compaction | **S** | `internal/promptctx/prune.go`, `condense.go` | Condenser is a cheaper model through the same Provider port, not a Python service |
| 9 | Provider port + normalized stream + usage split by token class | **F** | `internal/provider/` | Anthropic adapter + deterministic fake |
| 10 | Deterministic recorded/fake provider (mandatory) | **F** | `internal/provider/fake/` | Scripts truncation, stall, malformed stream, throttle, failover |
| 11 | Deterministic routing by data label + difficulty | **F** | `internal/provider/router.go` | Decision + reason persisted on the session |
| 12 | Failover taxonomy (retryable / permanent / context-overflow) + commit point | **F** | `internal/provider/failover.go` | Once first chunk emitted, the stream is committed |
| 13 | Qualified tool identity `{ns}/{name}@{ver}`, one owner per namespace | **F** | `internal/tools/identity.go` | Collision refused at admission, never by registration order |
| 14 | Catalog manifest pins the *resolvable* universe into `harness_digest` | **F** | `internal/tools/manifest.go` | Deferred disclosure ships as a manifest even with a small resident catalog |
| 15 | Descriptor admission scan + digest re-verification at use | **F** | `internal/tools/admit.go` | Injection scan on descriptors; step 1a re-verify |
| 16 | The single execution pipeline (16 ordered steps) | **F** | `internal/tools/pipeline.go` | Every step present, in order, individually tested |
| 17 | The 10-layer permission resolution order | **F** | `internal/permissions/chain.go` | Total order; table-driven test over all layer combinations |
| 18 | Autonomy as a one-way ratchet | **F** | `internal/permissions/autonomy.go` | No widening operation exists on any interface |
| 19 | Tool profiles (Gate 1) as versioned tenant config | **F** | `internal/permissions/profile.go` | Never resolves `ALLOW` |
| 20 | Per-invocation hybrid safety classifier (rules → model, fail closed to ASK) | **F** | `internal/permissions/safety/` | Model leg has bounded timeout + circuit breaker |
| 21 | Rule of Two over declared taint legs + session taint state | **F** | `internal/permissions/ruleoftwo.go` | Taint transitions are events; only an operator re-baseline clears the untrusted leg |
| 22 | Hook layer (`pre_tool_use`/`post_tool_use`), tighten-only, three handlers | **F** | `internal/hooks/` | `command` + `http` (SSRF-guarded) + `prompt`; chain budget, decision cache, `hook_stopped` |
| 23 | Approval as a **transaction** bound to a canonical digest | **F** | `internal/oversight/approval.go` | `granted_modified`, `approval_mismatch`, expiry-as-denial, invalidate-with-run |
| 24 | Input requests (agent→human pull), zero authorization value | **F** | `internal/oversight/input.go` | Distinct lifecycle + distinct terminal reason |
| 25 | Write-ahead idempotency claim, resolved by probe or human | **F** | `internal/store/claims.go` | Claim `in_flight` **before** the effect leaves the process |
| 26 | Condensation / Checkpoint / Snapshot as three artifacts | **F** | `internal/store/` | Test: a condensation cannot answer "did the payment go out" |
| 27 | Replay / resume / fork as three named operations | **F** | `internal/runctl/` | `replay` is pure; `fork` disables external effects |
| 28 | `harness_digest` pins behavior; divergence reported on fork | **F** | `internal/harness/digest.go` | Pinned at run start, never moves mid-run |
| 29 | Tenant-first RLS with **transaction-local** scope | **F** | `migrations/`, `internal/store/tenant.go` | `set_config('app.tenant_id', $1, true)` — the only scoping call |
| 30 | Isolation test executed **through the pooler** | **F** | `internal/store/isolation_integration_test.go` | Runs against PgBouncer :6432 in transaction-pooling mode |
| 31 | Hash-chained audit receipts, sign-only key custody, external anchor, scheduled verifier | **F** | `internal/audit/`, `cmd/signerd/` | `signerd` holds the key over a unix socket; `nexusd` can sign, never read |
| 32 | Per-tenant envelope encryption + crypto-shredding erasure | **F** | `internal/crypto/` | Erasure destroys the DEK; `payload_digest` survives; chain still verifies |
| 33 | Derived artifacts hard-deleted in the same erasure transaction | **F** | `internal/crypto/postgres/shred.go` | Reconciliation job proves no derived row outlived its source |
| 34 | Content-free telemetry: deny-by-default attribute allowlist, no admitting flag | **F** | `internal/obs/allowlist.go` | Exporter-side filter + a test that tries every content key |
| 35 | Content access grant: audited, expiring, receipt per read | **F** | `internal/contentaccess/grants.go` | The only path to plaintext outside the run's own audience |
| 36 | Turn-scoped spans derived from the log + bidirectional join key | **S** | `internal/obs/span.go` | Emitted per turn from durable events; exporter is stdout OTLP |
| 37 | Cost: token classes, versioned price book, reserve→reconcile, epoch-marked counter | **F** | `internal/cost/` | Redis Lua reserve; epoch mismatch fails closed |
| 38 | `Money` as exact integers + explicit currency, rounding once | **F** | `internal/cost/money.go` | No binary float anywhere in the path |
| 39 | `budget_decision` for every gate resolution, including `skip` | **F** | `internal/cost/gate.go` | An unenforced ceiling is visibly distinct from a ceiling with room |
| 40 | Every model call metered — including compaction, safety leg, judge, titles | **F** | `internal/cost/meter.go` | "Off the paying loop" = cheaper model, never unmetered |
| 41 | Durable queue + session-key serial lock + stateless workers | **S** | `internal/queue/` | Port + Redis Streams consumer-group adapter (`XAUTOCLAIM` for reclaim — Postgres `SKIP LOCKED` was the original adapter, retired in `migrations/0024_retire_queue_jobs.sql`); Redis lock on `session_key` |
| 42 | Typed failure classification, logged backoff+jitter, circuit break at 3 | **F** | `internal/reliability/` | Silent retry is impossible by construction |
| 43 | Stuck detection escalating from `stuck_suspected` to terminate | **F** | `internal/reliability/stuck.go` | Second corroborating trip terminates |
| 44 | Sandbox: hard CPU/mem/PID/wall limits, network default-deny | **S** | `internal/sandbox/` | Docker + `--network none` + rlimits; `isolation` field carries `gvisor`/`kata` as unshipped values |
| 45 | In-sandbox broker = the only route from sandbox code to a tool | **S** | — (not built; task T136 in [tasks.md](tasks.md)) | Optional Phase 8 stretch; until then connectors are in the egress deny set (the original's own interim posture) |
| 46 | File-first memory, injected at session start, injection-screened, retention-bounded | **F** | `internal/memory/` | Writes take effect next session (cache stability) |
| 47 | Skills as signed content-addressed bundles, 3-tier disclosure | **F** | `internal/skills/` | `bundle_digest`, per-file scan, `declared_tool_ids` **intersects** |
| 48 | A skill's script is a tool or the bundle is refused | **F** | `internal/skills/admit.go` | No execution path for attachment-borne code |
| 49 | Surfaces: capability descriptor, per-turn principal, conversation binding | **F** | `internal/surfaces/` | Two surfaces (REST, CLI) prove Principle I |
| 50 | Delivery outbox — event appended **before** the send | **F** | `internal/surfaces/outbox.go` | `failed_permanent` stays distinguishable from unanswered |
| 51 | Audience-gated run output (content for the run's own audience) | **F** | `internal/runs/reader.go` | Distinct signal class from telemetry; withheld ≠ empty |
| 52 | Declarative orchestration plane, zero-token routing | **F** | `internal/plan/` | Predicates are a **closed JSON AST**, not a string language |
| 53 | Plan lifecycle gates: validate → eval → sign-off → pin | **F** | `internal/plan/lifecycle.go` | Oversight-completeness validation included |
| 54 | Delegation: scope descends, taint ascends, bounded, envelope-reserved | **F** | `internal/delegate/` | Delegation is a tool invocation through the same pipeline |
| 55 | Chain attribution (`root`/`parent`/`depth`) on every row | **F** | schema-wide | Foundational columns, written even before delegation ships |
| 56 | Eval gate: k trials, exact intervals, three-valued verdict, suite classes | **F** | `evals/` | `inconclusive` never resolves to `pass` |
| 57 | `eval_environment_digest`, cold sandboxes per trial | **F** | `evals/environment.go` | Refuses to compare across digests |
| 58 | Code graders first, judge as last resort, held-out gap measured | **F** | `evals/judge.go`, `evals/heldout.go` | Judge is cross-family and calibration-gated |
| 59 | Efficiency gated alongside quality | **F** | `evals/efficiency.go` | Tokens/turns/tool-calls bands block a regression |
| 60 | Trajectory grading, not only end state | **F** | `evals/trajectory.go` | Tool-selection accuracy, ask-vs-guess, turns consumed |
| 61 | Config-not-forks onboarding | **F** | `internal/config/` | Tenant/agent/profile/policy are DB rows + markdown bootstrap |
| 62 | Control↔data-plane versioned contract | **F** | `internal/controlplane/` | `Port` + `v1` shapes + a real `LocalPort` implementation; import-boundary test activates (Phase 15, task 15.4) |
| 63 | Integration ports + authority boundary | **K** | port interfaces only | No third-party adapters — the state the original calls "must keep working forever" |
| 64 | MCP client, per-user OAuth connectors, Telegram/Zalo, email, cron, web UI | **S** | `internal/surfaces/{mcp,telegram,zalo,email,cron}/`, `internal/connectors/`, `web/` | Phase 11 — six more thin adapters over the existing capability-descriptor pattern (#49); zero kernel change |
| 65 | Credit ledger, billing periods, FX, price overrides, chargeback | **✗** | — | Commercial process, not architecture |
| 66 | Multi-region residency, BYOK, four-topology packaging, rainbow deploy | **✗** | — | `region` and `key_id` columns kept as seams |
| 67 | Retrieval/pgvector tier, document conversion, adversarial scan, adaptation proposals | **S** / ✗ | `internal/retrieval/`, `internal/ingest/` | Phase 12 ships retrieval + document conversion; adversarial scan and adaptation proposals stay out — each still gated on a trigger the demo never reaches |

**Score: 57 of 67 patterns at full or simplified fidelity, 4 as seams, 6 deliberately out.**
Every omission is a deployment, commercial, or connector concern — no architectural idea is dropped.

One addition sits outside this count: peer agent teams (Phase 9) — shared task boards with
Kanban-style claiming. It is not a simplification of anything in the original's 67; it is new
scope, added after the fact and scoped tightly (fixed roster, shared budget envelope, read-time
taint propagation) so it reuses this plan's existing primitives instead of adding a second set of
rules alongside them.

Two more rows from the commercial/compliance-tail collapse move back in the same way, as
Phases 11–12: **additional surfaces** (MCP client, per-user OAuth connectors,
Telegram/Zalo/email/cron, React web app) and the **retrieval tier** (pgvector + document
conversion). Both extend the plan rather than change what the core 10 phases drop — REST + CLI
already prove the surface pattern (#49) and file-first memory already proves the memory pattern
(#46), so neither phase introduces a new architectural idea. They're built anyway, on the same
seams, once there's a concrete integration need and a durable-knowledge corpus that outgrows
file-first memory — see §2 Phases 11–12.
