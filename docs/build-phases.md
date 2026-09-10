# Build phases and pattern coverage

Split out of [`README.md`](../README.md) — the architecture narrative lives there; this file is
the fidelity ledger (§1), the task-by-task build plan (§2), including Phases 13–15, added
2026-09-04 after [`docs/production-readiness-review.md`](production-readiness-review.md) found
that several patterns rated **F** below were not actually at full fidelity in the shipped code,
the effort estimate (§3), and the risk register (§4).

---

## 1. Pattern coverage map

Legend: **F** = full fidelity · **S** = simplified but structurally identical · **K** = seam/schema kept, behavior deferred · **✗** = out of scope.

| # | Pattern (original) | Demo | Where it lands | Note |
|---|---|---|---|---|
| 1 | Single kernel loop, async generator | **F** | `kernel/` via Go 1.23 `iter.Seq2` range-over-func | The generator seam survives translation exactly |
| 2 | Typed response classification (`TOOL_CALLS`/`CONTENT`/`EMPTY`) | **F** | `kernel/classify.go` | `exhaustive` linter enforces the switch |
| 3 | Nine typed terminal reasons + a named producer for each | **F** (8 of 9) | `kernel/terminal.go` | `credit_exhausted` dropped with the billing plane |
| 4 | Paired `tool_use`/`tool_result` invariant, synthetic on every error path | **F** | `kernel/hygiene.go` + property test | Property-based over generated histories, as the constitution requires |
| 5 | Append-only event log, versioned envelope, upcasting path | **F** | `internal/store/`, `migrations/` | 40+ event types, `schema_version`, upcast registry |
| 6 | Projections are never a second source of truth | **F** | `internal/store/project.go` | `status`, `terminal_reason`, `taint_state` rebuilt by replay; test asserts rebuild == stored |
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
| 30 | Isolation test executed **through the pooler** | **F** | `tests/integration/isolation_test.go` | Runs against PgBouncer :6432 in transaction-pooling mode |
| 31 | Hash-chained audit receipts, sign-only key custody, external anchor, scheduled verifier | **F** | `internal/audit/`, `cmd/signerd/` | `signerd` holds the key over a unix socket; `nexusd` can sign, never read |
| 32 | Per-tenant envelope encryption + crypto-shredding erasure | **F** | `internal/crypto/` | Erasure destroys the DEK; `payload_digest` survives; chain still verifies |
| 33 | Derived artifacts hard-deleted in the same erasure transaction | **F** | `internal/crypto/shred.go` | Reconciliation job proves no derived row outlived its source |
| 34 | Content-free telemetry: deny-by-default attribute allowlist, no admitting flag | **F** | `internal/obs/allowlist.go` | Exporter-side filter + a test that tries every content key |
| 35 | Content access grant: audited, expiring, receipt per read | **F** | `internal/obs/grant.go` | The only path to plaintext outside the run's own audience |
| 36 | Turn-scoped spans derived from the log + bidirectional join key | **S** | `internal/obs/spans.go` | Emitted per turn from durable events; exporter is stdout OTLP |
| 37 | Cost: token classes, versioned price book, reserve→reconcile, epoch-marked counter | **F** | `internal/cost/` | Redis Lua reserve; epoch mismatch fails closed |
| 38 | `Money` as exact integers + explicit currency, rounding once | **F** | `internal/cost/money.go` | No binary float anywhere in the path |
| 39 | `budget_decision` for every gate resolution, including `skip` | **F** | `internal/cost/gate.go` | An unenforced ceiling is visibly distinct from a ceiling with room |
| 40 | Every model call metered — including compaction, safety leg, judge, titles | **F** | `internal/cost/meter.go` | "Off the paying loop" = cheaper model, never unmetered |
| 41 | Durable queue + session-key serial lock + stateless workers | **S** | `internal/queue/` | Port + Postgres `SKIP LOCKED` adapter; Redis lock on `session_key` |
| 42 | Typed failure classification, logged backoff+jitter, circuit break at 3 | **F** | `internal/reliability/` | Silent retry is impossible by construction |
| 43 | Stuck detection escalating from `stuck_suspected` to terminate | **F** | `internal/reliability/stuck.go` | Second corroborating trip terminates |
| 44 | Sandbox: hard CPU/mem/PID/wall limits, network default-deny | **S** | `internal/sandbox/` | Docker + `--network none` + rlimits; `isolation` field carries `gvisor`/`kata` as unshipped values |
| 45 | In-sandbox broker = the only route from sandbox code to a tool | **S** | `internal/sandbox/broker.go` | Optional Phase 8 stretch; until then connectors are in the egress deny set (the original's own interim posture) |
| 46 | File-first memory, injected at session start, injection-screened, retention-bounded | **F** | `internal/memory/` | Writes take effect next session (cache stability) |
| 47 | Skills as signed content-addressed bundles, 3-tier disclosure | **F** | `internal/skills/` | `bundle_digest`, per-file scan, `declared_tool_ids` **intersects** |
| 48 | A skill's script is a tool or the bundle is refused | **F** | `internal/skills/admit.go` | No execution path for attachment-borne code |
| 49 | Surfaces: capability descriptor, per-turn principal, conversation binding | **F** | `internal/surfaces/` | Two surfaces (REST, CLI) prove Principle I |
| 50 | Delivery outbox — event appended **before** the send | **F** | `internal/surfaces/outbox.go` | `failed_permanent` stays distinguishable from unanswered |
| 51 | Audience-gated run output (content for the run's own audience) | **F** | `internal/surfaces/project.go` | Distinct signal class from telemetry; withheld ≠ empty |
| 52 | Declarative orchestration plane, zero-token routing | **F** | `internal/plan/` | Predicates are a **closed JSON AST**, not a string language |
| 53 | Plan lifecycle gates: validate → eval → sign-off → pin | **F** | `internal/plan/lifecycle.go` | Oversight-completeness validation included |
| 54 | Delegation: scope descends, taint ascends, bounded, envelope-reserved | **F** | `internal/delegate/` | Delegation is a tool invocation through the same pipeline |
| 55 | Chain attribution (`root`/`parent`/`depth`) on every row | **F** | schema-wide | Foundational columns, written even before delegation ships |
| 56 | Eval gate: k trials, exact intervals, three-valued verdict, suite classes | **F** | `evals/` | `inconclusive` never resolves to `pass` |
| 57 | `eval_environment_digest`, cold sandboxes per trial | **F** | `evals/digest.go` | Refuses to compare across digests |
| 58 | Code graders first, judge as last resort, held-out gap measured | **F** | `evals/grader/` | Judge is cross-family and calibration-gated |
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

---

## 2. Build phases

Seventeen phases (0 through 16). Each is independently shippable and ends with a **demo command**
you can run and an **acceptance test** that must be green. Sizing assumes one developer;
parallelisable work is marked `[P]`. Phases 13–16 are not part of the original 67-pattern coverage —
see their own intros below.

### Phase 0 — Setup (1 day)

| # | Task | File(s) |
|---|---|---|
| 0.1 | `go mod init github.com/truongpx396/nexus-agent-demo`; Go toolchain pinned in `go.mod` | `go.mod` |
| 0.2 | `docker-compose.yml`: postgres:17, pgbouncer (**transaction pooling**, :6432), redis:7 | `deploy/docker-compose.yml` |
| 0.3 | `golangci-lint` with `exhaustive`, `errcheck`, `gosec`, `forbidigo` (ban `SET app.tenant_id`, ban `float64` in `internal/cost`) | `.golangci.yml` |
| 0.4 | `Makefile`: `up`, `migrate`, `seed`, `run`, `test`, `eval`, `verify-chain` | `Makefile` |
| 0.5 | CI: build → lint → unit → integration (testcontainers) → **eval gate** | `.github/workflows/ci.yml` |
| 0.6 | Copy the constitution into `docs/constitution.md` — it is the review checklist | `docs/` |

**Acceptance**: `make up && make test` green on an empty test suite; `golangci-lint run` clean.

---

### Phase 1 — Foundational seams (5–6 days) ⚠️ blocking

> Everything here is a *schema or contract* decision. The original's cut line exists because
> each of these is expensive-to-impossible to retrofit onto a log that already has rows in it.

| # | Task | Proves |
|---|---|---|
| 1.1 | Migrations: `events` (with `schema_version`, `payload_digest`, `key_id`, `pair_ref`, `trace_id`/`span_id`), `sessions` (with `harness_digest`, fork columns, `root/parent/depth`, `plan_id`) | FR-006, FR-086, FR-119, FR-128, FR-129, FR-101 |
| 1.2 | RLS policies on every tenant table + append-only trigger on `events` (no UPDATE/DELETE) | Principle VI |
| 1.3 | `store.InTenantTx` — transaction-local scope, the only scoping call in the codebase | FR-039 |
| 1.4 | `[P]` **Isolation test through PgBouncer**: two tenants, interleaved statements on a pooled connection, cross-read must fail | FR-039 — the test that makes the claim real |
| 1.5 | Event taxonomy (~40 types) + envelope upcasting registry + a v0→v1 upcast test | FR-085, FR-086 |
| 1.6 | `crypto`: KEK from file → per-tenant DEK, AES-256-GCM seal/open, `payload_digest` over plaintext | FR-080, FR-089 |
| 1.7 | `obs`: deny-by-default attribute allowlist + a span exporter that drops unlisted keys | FR-117 |
| 1.8 | `[P]` `provider/fake`: YAML-scripted deterministic provider incl. truncation, stall, malformed stream, throttle | FR-097 — **mandatory** |
| 1.9 | `[P]` `evals/`: corpus loader, runner skeleton, code graders, CI gate wired to fail the build | Principle IX — **before the first behavior-bearing slice** |
| 1.10 | `harness.Digest()` over (system-prompt version, catalog manifest, skill set, safety policy, approval policy, prompt mode) | FR-129 |
| 1.11 | `tests/contract/boundaries_test.go` — import-graph assertions for the CP/DP split | Delivery constraint |

**Demo**: `make migrate && make seed TENANT=acme` prints *"RLS enabled on 32/32 tenant tables;
tenant scope is transaction-local"*.
**Acceptance**: 1.4 fails loudly if you change `set_config(...,true)` to `set_config(...,false)`.
That single-character sensitivity is the point of the phase.

---

### Phase 2 — The kernel loop (5 days) 🎯 first behavior-bearing slice

| # | Task | Proves |
|---|---|---|
| 2.1 | `kernel/loop.go` — `iter.Seq2[Event,error]` generator: hygiene → reserve → stream → classify → dispatch → pair → terminate | FR-001, FR-002 |
| 2.2 | `kernel/classify.go` — typed union, dispatch on classification, never on text | FR-002 |
| 2.3 | `kernel/terminal.go` — 8 reasons, each with a **named producer**; `exhaustive` linter on every switch | FR-004 |
| 2.4 | `kernel/hygiene.go` — drop orphan results, backfill synthetic results, prune stale observations | FR-060 |
| 2.5 | **Property test**: over generated event histories, every `tool_use` has exactly one `tool_result` before the next model call | FR-003, FR-097 |
| 2.6 | `promptctx`: two-zone builder — byte-stable prefix (stable system prompt + sorted resident catalog + append-only transcript) vs. volatile tail | FR-013 |
| 2.7 | **Prefix byte-equality test** across N turns + cache-read rate computed from recorded per-class token counts | FR-014, SC-003 |
| 2.8 | `provider`: port + normalized stream + Anthropic adapter + `router.go` (data label × difficulty, decision persisted) | FR-027, FR-037 |
| 2.9 | `provider/failover.go`: typed trigger taxonomy; **committed after first chunk**; never fail over on context overflow | FR-167 |
| 2.10 | REST surface: `POST /v1/runs`, `GET /v1/runs/{id}`, `GET /v1/runs/{id}/events` (SSE, audience-gated) | FR-031, FR-191 |

**Demo**: `curl -X POST localhost:8080/v1/runs -d '{"input":"..."}'` → `202`, then SSE streams
`content` / `tool_use` / `tool_result` / `terminal`.
**Acceptance**: kill the process mid-turn; the log still shows a paired result for every
`tool_use`. Change one byte of the system prompt mid-session in a test — the prefix test fails.

---

### Phase 3 — Tool pipeline + permission chain + hooks (7–8 days)

This is the densest phase and the one that most defines the platform.

| # | Task | Proves |
|---|---|---|
| 3.1 | `tools/identity.go` — `{ns}/{name}@{ver}`, one owner per namespace, **collision refused at admission** (never by registration order) | FR-147 |
| 3.2 | `tools/manifest.go` — catalog manifest pins the *resolvable* universe into `harness_digest`; `tool_loaded` lands in the volatile zone | FR-148 |
| 3.3 | `tools/admit.go` — descriptor injection scan (`pending` → `clean` / `flagged` / `rejected`, fail closed) | FR-113 |
| 3.4 | `tools/pipeline.go` — the 16 ordered steps, each individually unit-tested | FR-007, FR-010 |
| 3.5 | Canonical digest (RFC 8785 JCS over `digest_fields`) — **one artifact, three jobs**: approval binding, idempotency key, step-9a re-verification | FR-103, FR-071 |
| 3.6 | `permissions/chain.go` — the 10-layer total order; table-driven test over the layer cross-product | FR-111 |
| 3.7 | `permissions/autonomy.go` — pinned ratchet; assert **no widening function exists** on any exported surface | FR-111 |
| 3.8 | `permissions/profile.go` — Gate 1, versioned tenant config, resolves only DENY/DEFER | FR-176 |
| 3.9 | `permissions/safety/` — hybrid: deterministic rule pass in-process, then a model leg with bounded timeout, **fail closed to ASK**, circuit breaker | FR-009, FR-116 |
| 3.10 | `permissions/ruleoftwo.go` — taint legs default TRUE; session taint state as a projection; `taint_transition` events; only an operator re-baseline clears the untrusted leg | FR-033, FR-087 |
| 3.11 | `hooks/` — dispatcher, `command`/`http`(SSRF-guarded)/`prompt` handlers, matcher + JSON-AST `if_expr`, per-hook timeout (default `block`), chain budget, per-turn cap, decision cache, `updatedToolInput` through a path allowlist that **re-binds the digest** | FR-171, FR-166 |
| 3.12 | Builtin tools: `platform/file_read`, `file_write`, `file_search`, `shell`, `web_fetch` — each with a taint declaration and an effect class | FR-056–FR-059 |
| 3.13 | Result budgeting: cap/paginate ~25k tokens, spill to blob dir, return preview + *"do not infer success from the preview"* banner | FR-010 |

**Demo**: `nexusctl run --autonomy read_only "delete the build dir"` → refused at layer 3 with a
typed reason and an audit trail; the same run at `supervised` → suspends on an approval.
**Acceptance**: a test that asserts *"a standing scope cannot cause layer 6 or 7 to be skipped"*,
and a test that a hook returning `ALLOW` is treated as `DEFER`.

---

### Phase 4 — Cost governance (4–5 days)

| # | Task | Proves |
|---|---|---|
| 4.1 | `cost/money.go` — exact integer minor units, explicit currency, declared scale, rounding **once at the asserted boundary**; `forbidigo` bans `float64` in the package | FR-180 |
| 4.2 | `cost/meter.go` — `(meter, quantity, unit)` registry with `reservable` flag; token family emitted, non-token meters registered but unemitted | FR-179 |
| 4.3 | `cost/pricebook.go` — versioned, keyed `(meter, priced subject, effective range)`; historical cost stays reproducible | FR-084, FR-181 |
| 4.4 | `cost/gate.go` — **reserve → stream → reconcile**; Redis Lua atomic counter with an **epoch marker**; unknown epoch = unavailable = fail closed (never "no spend yet") | FR-083, FR-186 |
| 4.5 | Worker-local hard per-run budget enforced synchronously (a ceiling never depends on a round trip) | Delivery constraint |
| 4.6 | `budget_decision` event for every resolution — `allow` / `refuse_ceiling` / `degrade` / `skip` — with reason, resolved scope, deciding budget | FR-188, FR-190 |
| 4.7 | `Reconcile` accepts `UNREPORTED` → reconciles at full reserved worst case, flagged (an unreliable provider must not look free) | FR-185 |
| 4.8 | **Every** model call routed through the gate: compaction, the safety model leg, prompt hooks, the judge, title generation | FR-165 — "off the paying loop" = cheaper model, never unmetered |
| 4.9 | Cost records appended in the same transaction as the turn and shipped through the outbox | FR-124 |

**Demo**: set a $0.05 per-task ceiling → the run terminates `cost_exhausted` **before** the
overspending call, with a `budget_decision` naming the budget that refused.
**Acceptance**: an integration test firing 20 concurrent sessions against one tenant ceiling —
total spend must not exceed the ceiling (this is what post-hoc aggregation cannot deliver).

---

### Phase 5 — Trust surface (8–9 days)

| # | Task | Proves |
|---|---|---|
| 5.1 | `cmd/signerd` — holds the audit key, exposes `Sign(digest)` over a unix socket; `nexusd` can sign but **cannot read** the key | Sign-only custody |
| 5.2 | `audit/chain.go` — receipt per mutating action, hash-chained per session, over **digests** not plaintext (so a lawful redaction never breaks verification) | FR-040, FR-081 |
| 5.3 | `audit/anchor.go` + `audit/verify.go` — periodic head anchoring outside the writing system; scheduled verifier alerting on a break or a sequence gap | FR-081 |
| 5.4 | `crypto/shred.go` — erasure destroys the DEK; **derived artifacts hard-deleted in the same transaction**; reconciliation job proves no derived row outlived its source | FR-080, FR-162 |
| 5.5 | Erasure test: after shredding, the event log still **replays structurally** and the audit chain still **verifies** | The reconciliation of append-only with the right to erasure |
| 5.6 | `oversight/approval.go` — the full transaction: digest binding, decision-ready context package, named assignee, TTL, `granted` / `granted_modified` / `denied` / `expired` / `invalidated` | FR-036, FR-103–FR-108 |
| 5.7 | Step 9a digest re-verification → typed `approval_mismatch`, never a silent re-request | FR-103 |
| 5.8 | Durable suspend at **zero token cost**: checkpoint + evict, resume on the approval event; suspended interval excluded from every latency SLI | FR-036, FR-120 |
| 5.9 | `oversight/input.go` — schema-declared question, `on_expiry` → recorded default assumption or `input_expired`; carries **zero** authorization | FR-110 |
| 5.10 | `Invalidate` on cancel / terminal / reap / ceiling breach / steer-into-suspension, each releasing a paired synthetic result | FR-106 |
| 5.11 | `obs/grant.go` — content-access grant: audited, expiring, hash-chained receipt on grant **and on every read** | FR-118 |
| 5.12 | `sandbox/` — Docker exec, `--network none`, CPU/mem/PID/wall limits, per-session workspace; breach → terminate + reclaim; `isolation` field carries `gvisor`/`kata` as unshipped values | FR-047, FR-059 |
| 5.13 | Egress allowlist for `web_fetch`; connector/MCP endpoints in the sandbox **deny** set (no broker yet — a bypass is never the interim state) | FR-037, FR-149 |
| 5.14 | Adversarial oversight tests: injected attempts to simulate consent, widen autonomy mid-run, reach a gated effect through a standing scope — all must be refused **and audited** | SC-025 |

**Demo**: `nexusctl run "email the Q3 numbers to finance@…"` → suspends; `nexusctl approvals show`
renders recipient/subject/attachment digests (never a bare UUID); modify the recipient at grant
time → the tool executes the **approver's** input and the agent is not told it ran unmodified.
Then substitute an argument after the grant → `approval_mismatch`.
**Acceptance**: 5.5 and 5.14. If either is weak, the trust surface is decoration.

---

### Phase 6 — Reliability & the three state artifacts (5–6 days)

| # | Task | Proves |
|---|---|---|
| 6.1 | `queue/` — port + Postgres `SKIP LOCKED` adapter + admission control; worker pool pulls jobs | FR-046 |
| 6.2 | Session-key serial lock in Redis (per-session serial, cross-session concurrent) | FR-046 |
| 6.3 | `Checkpoint` — covered seq, open claim, held reservation, sandbox handle, pending approval digest, in-flight provider request id, open delegations, `harness_digest` | FR-024, FR-126 |
| 6.4 | `Snapshot` — disposable projection cache; **test: deleting every snapshot changes nothing but hydration time** | FR-126 |
| 6.5 | `Condensation` — model-facing only; **test: a condensation cannot answer whether an external effect completed** | FR-015, FR-130 |
| 6.6 | `claims`: write-ahead `in_flight` **before** the effect leaves the process; `completed` short-circuits; resume resolves by probe or human — **never** by re-execution, never by silent discard | FR-127 |
| 6.7 | `reliability/classifier.go` — typed failure classes before any retry; backoff + jitter **logged with a reason**; circuit break after 3 identical failures | FR-023 |
| 6.8 | `reliability/stuck.go` — repeated action / oscillation / zero net change over K steps → `stuck_suspected` (non-terminal), terminate only on a corroborating second trip | FR-115 |
| 6.9 | `runctl/` — `steer` (drained at a turn boundary under the serial lock; steering into a suspended run invalidates its approval), `cancel` (the sole producer of `aborted`), `resume`, `tightenAutonomy` | FR-005 |
| 6.10 | `replay` — **pure**: no model call, no tool, no append; how projections rebuild and how upcasting is verified | FR-128 |
| 6.11 | `fork` — new run from `at_seq` with declared overrides, external effects **disabled**, inherits no approvals, own budget and audit chain; reports digest divergence rather than presenting it as a reproduction | FR-128, FR-129 |

**Demo**: `kill -9` the worker mid-tool-call → the job re-queues and **resumes from the
checkpoint**, and the in-flight claim is escalated rather than re-executed.
Then `nexusctl fork <session> --at 42 --model haiku` reproduces a failure against a candidate fix.
**Acceptance**: 6.4, 6.5, 6.6 — the three artifacts must be provably non-interchangeable.

---

### Phase 7 — Harness growth: memory, skills, surfaces (5–6 days)

| # | Task | Proves |
|---|---|---|
| 7.1 | `memory/` — file-first per tenant, injected **at session start** (writes take effect next session), injection/exfiltration screening before injection, 90-day retention | FR-019 |
| 7.2 | Memory consolidation as an **ordered, metered, degrade-capable** stage: the durable write precedes the compaction that would discard its source; falls back to a no-model extractive pass at a ceiling | FR-165 |
| 7.3 | `skills/` — signed content-addressed bundle: `bundle_digest` over every file, per-file injection scan, three disclosure tiers (resident metadata → body on activate → per-file reference) | FR-020, FR-151 |
| 7.4 | `declared_tool_ids` **intersects** the resolved catalog, never unions; a non-held entry is ignored and recorded as `skill_capability_ignored` | FR-153 |
| 7.5 | A bundled script registers as a real tool through the ordinary gates **or the bundle is refused** | FR-151 |
| 7.6 | `internal/skills/manifest.go` — tier-1 resident metadata (`skill_id`, description, trigger hint, `declared_tool_ids`) for the tenant's admitted set, folded into `SkillSetDigest` at run start; **no `skill_search` tool** — the catalog is vetted and per-tenant-bounded, not a corpus (seam left for one if that stops being true) | FR-020, build-for-the-stage constraint |
| 7.7 | `internal/tools/builtin/skill.go` — `activate_skill(skill_id)` implements the ordinary `Tool` interface, **no new ABI**: dispatched through the same 16-step pipeline as any tool; its `tool_result` **is** the skill body, extending the byte-stable prefix the way every tool result already does — never a mid-session system-prompt splice, and `kernel/classify.go` needs zero changes (it is just another `TOOL_CALLS` turn) | FR-151 |
| 7.8 | Activation re-checks `declared_tool_ids` against the **currently resolved** catalog, not just what admission saw — tenant config can move between admission and activation; emits `skill_activated` (the live subset) and `skill_capability_ignored` per absent entry, fail closed | FR-153 |
| 7.9 | `read_skill_file(skill_id, path)` — lazy tier-3, reference/template content only, path-allowlisted like any sandboxed file read; a bundled **script** is never fetched this way — it is admitted as its own tool (7.5) and invoked as its own `tool_use` | FR-151 |
| 7.10 | `promptctx/prune.go` — non-destructive live pruning (outlier guard → soft trim → hard clear to a refetchable reference); **never mutates a logged event** | FR-164 |
| 7.11 | `promptctx/condense.go` — structured compaction at ~80% budget on a cheaper model through the same Provider port; metered; blocking-or-not is a **declared, measured** property | FR-015, FR-130 |
| 7.12 | `surfaces/` — capability descriptors (`principal_kind`, can-render-approval-context, step-up, structured input, streaming), conformance test per surface, approval routing **filters on capability** | FR-155 |
| 7.13 | Per-turn principal resolution — authority is the **turn-submitting** principal, never inherited from whoever opened the thread | FR-156 |
| 7.14 | `surfaces/outbox.go` — event appended **before** the send; at-least-once, idempotent on `(session, seq, surface, recipient)`; `failed_permanent` stays distinguishable from unanswered | FR-157 |
| 7.15 | `nexusctl` as a second real surface over the same kernel — zero kernel changes | FR-001 |

**Demo**: submit the same task through REST and through `nexusctl`; both produce identical event
sequences and identical terminal reasons. `git diff kernel/` after adding the CLI is empty.
Separately: activate a skill, then revoke one of its `declared_tool_ids` from the tenant catalog
mid-session and activate again — `skill_capability_ignored` fires, the run continues on the tools
it still holds, and `git diff kernel/` for the whole skills feature is likewise empty.
**Acceptance**: 7.4 (a skill cannot widen capability), 7.7 (skill activation adds no kernel control
flow — it is dispatched, paired, and appended exactly like any other tool call), and 7.15 (the
empty diff).

---

### Phase 8 — Orchestration plane + delegation (6–7 days)

| # | Task | Proves |
|---|---|---|
| 8.1 | `plan/schema.go` — `Plan{steps[], cost_envelope}`, `Step{kind}` ∈ agent / delegate_fanout / approval_gate / preauth / input_request / condition / loop | FR-102 |
| 8.2 | `plan/predicate.go` — a **closed JSON AST** (eq, ne, lt, gt, and, or, in, typed field refs). No string eval, no I/O, no model call, no unbounded loop | The zero-token claim becomes a *property* |
| 8.3 | `plan/validate.go` — schema, reachability, bounded loops, closed predicates, scope-subset proof per step, and **oversight completeness** (a plan that routes around its own tenant's approval policy fails validation) | FR-102 |
| 8.4 | Lifecycle: `draft → validate → eval gate → governance sign-off → enabled`; pins `agent_version` + `route_model_id` at enable; enabled versions immutable; in-flight runs finish on their version | FR-088, FR-096 |
| 8.5 | `plan/exec.go` — platform evaluates transitions; step boundary = checkpoint; `plan_started`/`step_entered`/`transition`/`step_exited`/`plan_completed` events | FR-024, FR-085 |
| 8.6 | **Zero-token routing test** against the fake provider: assert *no* `Provider.Stream` call occurs while evaluating a transition | SC-023 |
| 8.7 | `preauth` step — enumerated, digest-bound set for one human decision; a preauth admitting anything outside its enumeration fails validation | FR-109 |
| 8.8 | `delegate/` — delegation is a **tool invocation through the same pipeline**: paired result, permission chain, audit receipt, all inherited | FR-100 |
| 8.9 | `internal/tools/builtin/delegate.go` — `delegate` implements the ordinary `Tool` interface, **no new ABI**: input `{agent_id, task, scope_grant, return_schema}`; `CheckPermissions` re-derives `scope_grant` as a provable subset of the **parent's own resolved scope** — it is never trusted from the input; `Taint()` defaults all three legs to `TRUE` like every tool, so autonomy level and the Rule of Two gate a delegation call **in addition to**, not instead of, the depth/concurrency/per_run bounds below | FR-100 |
| 8.10 | Delegation suspends on the **same durable-suspend primitive** approvals already use (5.8) — checkpoint + evict, resume on `delegation_returned` / `delegation_reaped` — at zero token cost; no second suspension mechanism is built | FR-100, FR-120 |
| 8.11 | Taint-ascend, made mechanical: a child's `taint_state` **starts as a copy of the parent's** at spawn (it inherits the trust context it was spawned from); on return, the parent's `taint_state` projection folds in the child's **own event-derived** `taint_state` — read from the child's history, never from a claim in its return payload, so a child cannot self-report "clean" | FR-098 |
| 8.12 | Scope descends (provable subset, no widening parameter), taint ascends (a summary never clears the untrusted leg), bounds fail closed (`depth ≤ 1`, `concurrent ≤ 3`, `per_run ≤ 16`) | FR-098, FR-099 |
| 8.13 | Fan-out cost reserved **as an envelope before the first child starts**; children draw from it (per-child reservation against the tenant counter is prohibited). A single ad hoc `delegate` call is not a fan-out — it reserves normally through `BudgetGate.Reserve`; the envelope applies specifically to a `delegate_fanout` plan step, sized for its worst-case child count up front | FR-099 |
| 8.14 | Reaping on parent terminal/cancel/ceiling; return-schema validation + acceptance criterion before folding in; `bound_exceeded` is **non-retryable** | FR-100 |
| 8.15 | `[P] stretch` — `sandbox/broker.go`: the only route from sandbox code into the pipeline; re-enters at step 1; the calling program **blocks** on the same durable suspension | FR-149 |

**Demo**: a 5-step "triage → draft → approve → send → record" plan runs twice and takes the same
branch both times; the transition log names the predicate that fired; total routing tokens = 0.
**Acceptance**: 8.6 and 8.12. Without them this is a workflow engine, not the pattern. 8.11 is the
one that keeps it from being a laundering vector: a child that quietly touched untrusted input
cannot return a clean-looking summary and erase that fact from the parent.

---

### Phase 9 — Peer agent teams: shared task boards (6–7 days)

Not derived from the original's 67 patterns — this is new scope, added deliberately outside that
fidelity map (§1's score stays 55/67; see the note there). It borrows every primitive it
can from what already exists — queue claim semantics, the session model, envelope reservation,
taint-fold — rather than inventing new mechanism, and is bound by the same fail-closed,
no-widening discipline as delegation (Phase 8).

| # | Task | Proves |
|---|---|---|
| 9.1 | `internal/teams/` — `Team{team_id, tenant_id, roster []AgentID, budget_envelope_id, status}`; **roster is fixed at creation**, no mid-run recruitment — the same no-widening discipline as the autonomy ratchet (3.7) and skill intersection (7.4) | New scope, bounded like delegation |
| 9.2 | `sessions.team_id` (nullable) + `delegation_role = 'team_member'`; each member is an **ordinary session** — reuses the session-key serial lock (6.2) for per-member concurrency, no new locking primitive for the loop | Schema-additive over Phase 1 |
| 9.3 | `board_cards` — RLS-scoped: `status ∈ {open, claimed, in_progress, done, blocked}`, `taint_state` copied from the writer at creation (8.11's copy-at-spawn pattern), `injection_scan_status ∈ {pending, clean, flagged}` | Same admission discipline as skills (7.3), applied to a new artifact |
| 9.4 | `claim_card` — the **same Postgres `SKIP LOCKED`** claim query `internal/queue/` already runs for job dispatch (6.1); no new concurrency primitive invented | Reuses 6.1 |
| 9.5 | `read_board` / `claim_card` / `write_card` / `update_card_status` — four ordinary `Tool`s through the same 16-step pipeline; `Taint()` defaults all-`TRUE` like every tool, so autonomy level and the Rule of Two gate board actions exactly as they gate `delegate` (8.9) | No new ABI |
| 9.6 | **Read-time taint fold** — the one genuinely new mechanism this phase needs: reading a card folds its `taint_state` into the reader's own `taint_state` projection, same shape as delegation's return-time fold (8.11) but triggered by a read instead of a return | Closes the laundering path a shared board would otherwise reopen |
| 9.7 | `write_card` scans the body through the **same injection/exfiltration scanner memory already uses** (7.1) before flipping `injection_scan_status` to `clean`; a `flagged` card is never surfaced to another peer's context — fail closed | Reuses 7.1 |
| 9.8 | Shared budget envelope reserved **once at team creation**, sized to the roster's worst case — 8.13's fan-out-envelope pattern, scoped to the team's lifetime instead of one plan step; no member draws an independent per-tenant reservation | Reuses 8.13 |
| 9.9 | Team lifecycle: `active → completed / aborted / ceiling_exhausted`; completes when the board has no `open`/`claimed` cards **and** every member is terminal, or the envelope exhausts (`cost_exhausted`), or a wall-clock backstop trips; termination **reaps every still-active member**, same as a delegation parent reaps children (8.14) | Reuses 8.14's reaping discipline |
| 9.10 | A team member is a **leaf**: it cannot itself spawn a delegation child or create a nested team — no recursive teams, no depth workaround through a side door | Preserves depth ≤ 1 (8.12) |

**Demo**: spin up a 3-member team against a 6-card board; two members race to claim the same card
under a concurrency test — exactly one wins, proven by the `SKIP LOCKED` claim, never a double
claim. A card written under a tainted session is read by a clean member, whose own `taint_state`
picks up the untrusted leg. The team completes when the board empties; total spend across all
three members never exceeds the envelope reserved at creation.
**Acceptance**: 9.4 under a contention test (no card is ever double-claimed) and 9.6 (a clean
reader is provably tainted by reading a tainted card — the same class of test 8.11 runs for
delegation, aimed at the new read-time path instead of the return path).

---

### Phase 10 — Eval gate hardening + go-live (4–5 days)

| # | Task | Proves |
|---|---|---|
| 10.1 | Corpus of ~20 cases split into **regression / capability / safety / negative** classes with distinct thresholds; **safety admits no threshold below 100%** | FR-137 |
| 10.2 | **k trials per case**, per-case Wilson intervals, regression defined as **interval separation** (not a flipped trial), three-valued verdict where `inconclusive` **never** resolves to `pass` | FR-138 |
| 10.3 | `eval_environment_digest` (image, resource bands, concurrency, region); **refuses to compare across digests**; trials run on **cold** sandboxes, never the warm path | FR-139 |
| 10.4 | Grader selection rule: **deterministic code graders wherever the criterion is objectively checkable**; the judge reserved for genuinely subjective criteria | FR-141 |
| 10.5 | Judge is a **pinned, cross-family** snapshot, calibrated against human labels to a published agreement floor **before** it may block a change | FR-141 |
| 10.6 | Held-out graders outside the agent's reach; the **visible-vs-held-out pass-rate gap is measured** — a widening gap is how spec-gaming announces itself | FR-141 |
| 10.7 | **Trajectory grading**: tool-selection accuracy, whether an input request was raised instead of a guess, turns and calls consumed | FR-142 |
| 10.8 | **Efficiency gated, not just reported**: a change holding its quality verdict while regressing tokens/turns/tool-calls past the declared band is **blocked** | FR-144 |
| 10.9 | Mandatory HITL adversarial cases: suppress or simulate consent, widen autonomy mid-run, reach a gated effect via a standing scope — all refused and audited | FR-112 |
| 10.10 | Per-artifact case sets: each skill, tool, plan, **and team roster** ships its own versioned cases, run at its promotion/enable gate | FR-143 |
| 10.11 | CI: `≥90% pass AND zero regressions` blocks merge on any prompt/tool/model/skill/plan/team change | FR-042, FR-043 |
| 10.12 | Golden-signal dashboard: completion rate by terminal reason, cost-ceiling breach rate, stuck rate, **cache-read rate**, approval time-to-decision, `approval_mismatch` rate, unresolved in-flight claims, telemetry attribute-drop rate, held-out gap | FR-095 |
| 10.13 | `docs/go-live.md` — the checklist, with a script that verifies each item against a live deployment | FR-045 |

**Demo**: `make eval` prints a per-case table with intervals and a three-valued verdict, then
`PASS (18/20, 0 regressions, efficiency within band)` or a red gate naming the case.
**Acceptance**: change a prompt so quality holds but tokens rise 40% → the gate **blocks**. That
is the whole point of 10.8, and the failure mode the original calls "an incident that ships with
a green check."

---

### Phase 11 — Additional surfaces: MCP client, per-user OAuth connectors, Telegram/Zalo/email/cron, React web app (8–9 days)

Not derived from new architecture — pattern #64 is already proven by REST + CLI (Phase 2, 7). This
phase is six more instances of the same capability-descriptor seam (#49), built once a concrete
integration need exists rather than deferred indefinitely (README.md §5's trigger for this row has fired).
Nothing here changes `kernel/`; 7.15's "empty kernel diff" proof extends to every surface added below.

| # | Task | Proves |
|---|---|---|
| 11.1 | `internal/surfaces/mcp/` — MCP client adapter: each remote MCP server's tools are qualified as `mcp/{server}/{tool}@{version}` and admitted through the ordinary identity (#13), manifest (#14), and descriptor-scan (#15) path | No new tool ABI; an external tool is exactly as trusted as a builtin one, never more |
| 11.2 | `internal/connectors/oauth.go` — per-user OAuth token vault: authorization-code flow per `(tenant_id, user_id, provider)`; tokens sealed under the existing per-tenant envelope encryption (#32), readable only inside a tool's `Call`, never placed in an event payload, log, or span | Reuses #32 and the allowlist discipline of #34 for a new content class |
| 11.3 | Connector tools declare `Taint().reads_private_data` / `mutates_external` per action like any other tool — gated by the permission chain (#17) and Rule of Two (#21) unchanged | No connector-specific carve-out in the chain |
| 11.4 | `internal/surfaces/telegram/`, `internal/surfaces/zalo/` — webhook surfaces; each declares a capability descriptor (#49), resolves the per-turn principal from the inbound sender (#49), replies through the existing outbox (#50) | Zero kernel change; reuses outbox at-least-once/idempotent delivery |
| 11.5 | `internal/surfaces/email/` — inbound via provider webhook or IMAP poll, outbound through the outbox; `failed_permanent` stays distinguishable from unanswered (#50) | Reuses #50 unchanged |
| 11.6 | `internal/surfaces/cron/` — a scheduler surface: a synthetic `principal_kind=scheduler` submits a run on a fixed schedule through the ordinary queue admission (#41) | No new admission path |
| 11.7 | `web/` — React app against the existing `POST /v1/runs` + SSE (`GET /v1/runs/{id}/events`, #2.10); renders approval context (digests, never a bare UUID, per Phase 5's demo), taint state, terminal reasons | No backend surface change — proves the API was surface-agnostic all along |
| 11.8 | Conformance test suite (#7.12) extended to all eight surfaces (REST, CLI, MCP, Telegram, Zalo, email, cron, web) | Same per-surface capability test, now run eight times |
| 11.9 | Egress allowlist (#5.13) extended per connector/provider host; connector traffic from inside the sandbox stays in the deny set (#5.13, #8.15) — the in-sandbox broker, if it ever ships, remains the only route | No bypass introduced by adding connectors |

**Demo**: the same task submitted via Telegram, email, and the React web app produces identical
event sequences and terminal reasons to REST/CLI. An MCP-provided tool is admitted, then denied at
permission layer 4 for a tenant whose tool profile excludes it, audited identically to a builtin
tool refusal.
**Acceptance**: 11.8 across all eight surfaces, and a test that greps every event payload, log
line, and span for a live OAuth token — none ever appears (the same class of test #34 runs for
telemetry, aimed at a new secret class).

---

### Phase 12 — Retrieval tier (pgvector) + document conversion (5–6 days)

Ships once file-first memory (#46) is actually exhausted (~1M tokens of durable per-tenant
knowledge, README.md §5's original trigger) rather than pre-built speculatively. Retrieval sits **beside**
memory, not instead of it — the same injection-screening and tenant-scoping discipline just
applies to a second store.

| # | Task | Proves |
|---|---|---|
| 12.1 | `internal/ingest/` — document conversion: PDF/DOCX/HTML/plaintext → normalized text; deterministic per-document digest; chunking with declared, stable chunk boundaries | Reuses the digest-over-plaintext idea from #32 |
| 12.2 | `internal/ingest/admit.go` — a converted document runs the same admission-scan discipline as a skill bundle or board card (`pending → clean/flagged/rejected`, fail closed) before a chunk is ever indexed | Reuses #15/#48/9.7's injection-scan pattern for a new artifact |
| 12.3 | `internal/retrieval/` — pgvector index: `(tenant_id, doc_id, chunk_id, embedding, source_digest)`; RLS + `InTenantTx` scoping (#29), no exception for embeddings | Reuses #29 with zero new scoping mechanism |
| 12.4 | Embedding calls routed through the same `Provider` port and `BudgetGate.Reserve` (#37, #40) — indexing is metered, never off the paying loop | Extends #40's "every model call metered" to embeddings |
| 12.5 | `provider/fake` extended with a deterministic embedding fake | Extends #10's mandate: no correctness test calls a live embedding model |
| 12.6 | `platform/retrieve` — an ordinary `Tool` through the 16-step pipeline (#16); `Taint().reads_private_data=true`; result budgeted/paginated like any tool result (#3.13) | No new ABI — retrieval is a tool, not a kernel primitive |
| 12.7 | Retrieved chunks pass through the same injection-screening as memory (#46) before entering the prompt | Reuses #46's screening for a second knowledge source |
| 12.8 | Erasure test extended (#33, 5.4–5.5): shredding a tenant's DEK makes its retrieval index unrecoverable too — indexed chunks are a derived artifact, hard-deleted in the same erasure transaction | Retrieval must obey crypto-shredding, not open a durable-knowledge side door around it |

**Demo**: ingest a 200-page PDF; `retrieve("...")` returns ranked chunks with source digests
through the same event-logged, permission-gated, budgeted path as any other tool call. Then erase
the tenant — the retrieval index is empty and the reconciliation job (#33) finds nothing
outstanding.
**Acceptance**: 12.8, and a test that no embedding call bypasses `BudgetGate.Reserve` (the same
AST-level check #4.8 already runs, extended to the embedding call site).

### Phase 13 — Production hardening: exposure and wire-protocol correctness (8–9 days)

Not part of the original 67-pattern coverage — nothing here is a new architectural idea. This
phase closes the gap between what §1's fidelity column claims and what the shipped code actually
does, found by an independent audit against `HEAD` after Phase 12 shipped
([`docs/production-readiness-review.md`](production-readiness-review.md), 2026-09-04, findings
F1–F15). It covers the findings that gate whether the binary may be exposed to real traffic at all
(F1–F3) plus the model-layer wire-protocol defects underneath it (F4, F5, F7, F8) — the audit's own
words, "fixing F1–F8 is roughly two weeks of unglamorous work." Phases 14–15 close the remaining
findings and depend on nothing here beyond the shared codebase. 13.4 blocks 13.7; 13.1–13.3 gate
whether any of this may run against real traffic; the rest are independent and marked `[P]`.

| # | Task | Closes |
|---|---|---|
| 13.1 | `[P]` Real per-tenant AuthN: a signed-JWT dev issuer (target: per-tenant OIDC) replaces the `X-Nexus-Tenant-ID`/`X-Nexus-User-ID` header read; the principal comes only from verified claims | F1 — the `AuthN` box README.md §3's diagram already draws but never wires |
| 13.2 | `[P]` `http.Server` with `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`/`IdleTimeout`; `signal.NotifyContext` driving `Shutdown(ctx)` so every `defer` in `serve()` (queue workers, cron, audit-anchor loop, team backstop) actually runs; `/healthz` (liveness) split from `/readyz` (Postgres + Redis + signerd socket reachable) | F2 |
| 13.3 | `[P]` Multi-stage distroless `Dockerfile` for `nexusd` + `signerd` (migrations already embedded via `migrations/embed.go`, so the image is self-contained); a compose profile running both against the existing infra services in `deploy/docker-compose.yml` | F3 |
| 13.4 | `provider.Message` carries typed content blocks with `tool_use_id` instead of a flat string — restores the paired `tool_use`/`tool_result` invariant (#4) all the way to the wire, not just inside the kernel; blocks 13.7 | F5 |
| 13.5 | `[P]` `System` becomes `[]{type,text,cache_control}` with an ephemeral breakpoint closing the stable zone; integration test asserts `InputCacheRead > 0` on turn two of a multi-turn session | F4 — the wire-level half of #7 that was never sent |
| 13.6 | `[P]` Price book seed drops `WildcardSubject` for one entry per routed model (`claude-haiku-4-5`, `claude-sonnet-5`, `claude-opus-5`) at real per-token rates, plus a distinct cache-read price | F8 — #37's price book, priced correctly and not just structured correctly |
| 13.7 | `thinking: {type:"adaptive"}` on every request; `stop_reason: "refusal"` maps to its own `DoneReason` carrying `stop_details.category`, never silently folded into `DoneStop` | F7 — depends on 13.4 |

**Demo**: `docker compose -f deploy/docker-compose.yml --profile app up` starts `nexusd` +
`signerd` from built images against the existing infra services; `curl /readyz` returns 200 only
once Postgres, Redis, and the signerd socket all answer; a request with no bearer token is 401,
never routed.
**Acceptance**: `cache_read_input_tokens > 0` on turn two of any multi-turn session;
`docker run --rm <image> id` reports a non-root uid for both the `nexusd` and `signerd` images.

### Phase 14 — Production hardening: safe to change (4 days)

Continues Phase 13's closure of `docs/production-readiness-review.md`'s findings; still no new
architectural idea. Closes the findings that make the rest of the codebase safe to keep changing —
test coverage, a live-model eval track, dependency scanning, and startup config validation. All
four tasks are independent of each other and of Phase 13/15 beyond the shared codebase, and are
marked `[P]`.

| # | Task | Closes |
|---|---|---|
| 14.1 | `[P]` Table-driven unit tests for `kernel/classify.go` and `kernel/terminal.go` (every arm, no Docker required); `internal/oversight`, `internal/audit`, `internal/delegate`, `internal/runctl` raised off 0.0% coverage | F9 |
| 14.2 | `[P]` 10–15 live-model task-completion cases behind `-tags=liveeval`, graded by the code graders `evals/` already ships; the scripted `provider/fake` cases stay as the harness suite, not repurposed as the eval gate | F6 |
| 14.3 | `[P]` `govulncheck ./...` as a CI job; `dependabot.yml` for `gomod`/`npm`/`github-actions` | F11 |
| 14.4 | `[P]` A validated `Config` struct read once at startup, failing closed on anything unset outside an explicit `--dev` mode; KEK generation gated the same way — a missing `NEXUS_KEK_PATH` outside `--dev` is fatal, never a silently generated new key | F12 |

**Demo**: `nexusd serve` with no env vars set and no `--dev` flag exits immediately with one
combined error listing every missing required setting, instead of silently generating a throwaway
key.
**Acceptance**: `go test -race -cover ./...` shows no package under `internal/` at 0.0%;
`make eval` (14.2's live-model cases included, when `-tags=liveeval` is set) green;
`govulncheck ./...` clean.

### Phase 15 — Production hardening: operable and defensible (4–5 days)

Closes the remaining findings from the same audit: real span export, sandbox hardening, measured
scale limits, and the `internal/controlplane` package README.md §2's collapse table already names.
Independent of Phases 13–14 beyond the shared codebase; all four tasks are independent of each
other and marked `[P]`.

| # | Task | Closes |
|---|---|---|
| 15.1 | `[P]` OTLP exporter behind the existing `obs.Exporter` interface — #36 upgraded from stdout to a real sink, the filtering guarantee unchanged; the golden-signal dashboard's queries exposed on `/metrics` | F13 |
| 15.2 | `[P]` `CapDrop: ["ALL"]`, `ReadonlyRootfs: true`, `SecurityOpt: ["no-new-privileges"]`, and a non-root `User` on every sandbox container, alongside #44's existing `--network none` and resource limits | F10 |
| 15.3 | `[P]` `pgxpool.ParseConfig` with explicit sizing reconciled against PgBouncer's pool; `bufio.Scanner.Buffer` raised past the 64KB default on the SSE reader; one load test against `POST /v1/runs` on `provider/fake` to put a measured number on the queue/pooler ceiling | F14 |
| 15.4 | `[P]` `internal/controlplane`: the `Port` interface + `v1` request/response shapes README.md §2's collapse table already names; the two rules in `tests/contract/boundaries_test.go` that currently `t.Skipf` on this package activate | F15 — #62 moves from **K** to actually kept |

**Demo**: a sandboxed tool container run with `docker inspect` shows dropped capabilities, a
read-only rootfs, and a non-root user; the OTLP exporter delivers spans to a real collector with
no content-bearing attribute ever leaving the process.
**Acceptance**: the load test in 15.3 reports a measured req/s number (not an assumed one);
`tests/contract/boundaries_test.go`'s two previously-`t.Skipf`'d rules for `internal/controlplane`
now run and pass.

---

### Phase 16 — Agentic capability surface: Crawl4AI, OpenSandbox, and a real skill/subagent bootstrap

Not part of the original 67-pattern coverage — like Phases 9 and 13–15, no new architectural idea
ships here. The point of this phase is the opposite: prove the seams §1 already rates **F**/**S**
by filling two of them with real, named open-source projects instead of a first-party stub, and by
actually populating two directories (`internal/skills`' bundle root, and the `delegate` tool's
`agent_id`/`scope_grant` convention) that have shipped as empty machinery since Phases 7–8. Two
upstream integrations, chosen because each lands on a seam this plan already left open on purpose:

- **[Crawl4AI](https://github.com/unclecode/crawl4ai)** (`unclecode/crawl4ai`) — an LLM-friendly web
  crawler with a Dockerized FastAPI server — becomes `platform/web_crawl`, a sibling of
  `platform/web_fetch` (#56 in §1) that returns rendered, boilerplate-stripped Markdown instead of
  raw HTML. It is **not** a new architectural idea: same taint declaration, same egress allowlist
  (task 5.13/11.9), same "content is untrusted" posture (Principle V) `platform/web_fetch` already
  has — the new capability is JS-rendered, LLM-ready extraction, not a new trust boundary.
- **[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox)**
  (`opensandbox-group/OpenSandbox`) fills the seam pattern #44's own doc comment names verbatim:
  *"Isolation carries gvisor/kata as unshipped values ... a stronger isolation backend is a config
  change later, not a schema or interface change."* `NEXUS_SANDBOX=opensandbox` becomes a second
  `sandbox.Isolation` alongside `docker`, satisfying the exact same `tools.SandboxExec` structural
  interface `sandbox.SessionSandbox` already does — `platform/shell` gets a stronger, actively
  maintained isolation backend (deny-by-default `NetworkPolicy`, a real lifecycle/execd/egress API)
  with **zero change** to `internal/tools`, the permission chain, or the kernel. No second tool, no
  new ABI — the seam was built for exactly this.

Both integrations run as opt-in services in a **third** compose file
(`deploy/docker-compose.agentic.yml`), the same append-only pattern
`deploy/docker-compose.local-llm.yml` (`docs/local-llm.md`) already established: nothing in it
references postgres/pgbouncer/redis/signerd/nexusd, so `make down` and `make llm-down` can never
touch it, and a demo that never runs `make agentic-up` pays nothing.

| # | Task | Proves |
|---|---|---|
| 16.1 | `internal/tools/builtin/web_crawl.go` — `platform/web_crawl(url)`: calls Crawl4AI's Docker server `POST /md` (`f=fit` boilerplate-stripped Markdown), `Authorization: Bearer` against `NEXUS_CRAWL4AI_API_TOKEN`; same `hostAllowed`/egress-allowlist check against the **target** URL as `platform/web_fetch` (task 5.13), same result-budget cap (task 3.13) | A second content-acquisition tool sits beside #56 without a second trust model — taint, egress allowlist, and untrusted-content handling are identical, only the extraction quality differs |
| 16.2 | `Taint{ReturnsUntrusted:true, MutatesExternal:true, ReadsPrivateData:false}` — identical to `platform/web_fetch`'s own declaration, not a weaker one, even though the actual HTTP fetch now happens inside Crawl4AI's container rather than in-process | Delegating the fetch to another service never delegates the trust decision (Principle V) |
| 16.3 | `internal/sandbox/opensandbox.go` — `OpenSandbox` backend implementing the same `Exec(ctx, cmd) (output, exitCode, breach, err)` shape as `sandbox.SessionSandbox` (structurally, no shared interface declaration — the existing decoupling idiom `internal/tools.SandboxExec`'s own doc comment names), via the official Go SDK (`github.com/alibaba/OpenSandbox/sdks/sandbox/go`): `opensandbox.CreateSandbox` with `NetworkPolicy{DefaultAction:"deny"}` (the same default-deny posture as Docker's `--network none`, task 5.12/5.13), `ConnectionConfig.UseServerProxy:true` so a host-run `nexusd` never needs the server's dynamic sandbox-port range published | Pattern #44's "config change later, not an interface change" claim, redeemed with a real second backend |
| 16.4 | `newSandboxFactory` (`cmd/nexusd/main.go`) grows an `NEXUS_SANDBOX=opensandbox` branch beside the existing `docker` one — same zero-setup discipline: unset stays unsandboxed, a configured-but-unreachable backend logs a warning and falls back, exactly like `NEXUS_SANDBOX=docker` already does when no daemon answers | The `docker`/`opensandbox` choice is operator config, never a code fork (pattern #61's own discipline, applied to isolation backends) |
| 16.5 | `deploy/docker-compose.agentic.yml` — `crawl4ai` (pinned `unclecode/crawl4ai:0.9.3`, port 11235, `CRAWL4AI_API_TOKEN` dev secret, `shm_size: 1gb`) and `opensandbox-server` (official `opensandbox/server:latest`, port 8090, `/var/run/docker.sock` mounted, `server.api_key` dev secret, `[docker] host_ip = "host.docker.internal"` — the config the project's own example compose file ships for exactly this "server runs in a container, sandboxes are its Docker siblings" topology) | Two more third-party adapters wired the way `docs/local-llm.md` already proved: opt-in, additive, independently torn down |
| 16.6 | `Makefile`: `agentic-up`/`agentic-down` (mirrors `llm-up`/`llm-down`); `docs/agentic-capabilities.md` (mirrors `docs/local-llm.md`'s voice): architecture, env vars, and an explicit **caveat** section — Crawl4AI's own JS-rendering cost, and OpenSandbox's server needing the Docker socket (a privileged deploy concern, named rather than hidden, the same honesty `docs/local-llm.md`'s own Caveats section already models) | Every prior third-party adapter in this codebase ships with the same shape of doc; this one does too |
| 16.7 | `.dev/skills/web-research/` and `.dev/skills/sandboxed-code/` — two real `skill.json` bundles (not test fixtures) with a `trigger_hint`, `declared_tool_ids` naming the tools above, and a tier-3 `GUIDE.md` reference file, loaded by the existing `skills.LoadBundles(NEXUS_SKILLS_ROOT)` (task 7.3) with no loader change; `make seed` admits both into the demo tenant's `admitted_skill_ids` (task 7.6) | Answers directly: yes, this codebase already has a "skills markdown" mechanism (task 7.3–7.9) — it has simply never been populated with real content until this task |
| 16.8 | Two named `agent_id`/`scope_grant` conventions over the **existing, unmodified** `platform/delegate` tool (#54/8.9) — `web-researcher` (`scope_grant: ["platform/web_crawl@v1","platform/web_fetch@v1","platform/activate_skill@v1"]`) and `sandbox-runner` (`scope_grant: ["platform/shell@v1","platform/activate_skill@v1"]`) — documented in `docs/agentic-capabilities.md`, referenced by both skills' `trigger_hint` text and by the UI's suggested prompts (16.9) | Deliberately **not** a new persona/system-prompt-override table: `scope_grant` (a provable tool-ref subset, task 8.9) already is what defines a sub-agent's capability surface in this architecture, and `harness_digest`/prompt cache-stability (#7, #28) make a per-agent system-prompt fork exactly the kind of thing this plan's own "seams decided early, never bolted on later" rule warns against — the honest move is documenting the existing mechanism, not adding one |
| 16.9 | `web/src/lib/suggestedPrompts.ts` + chip UI on `NewRun` (populates the initial input) and `RunDetail`'s steer box (injects a follow-up turn) — one-click prompts exercising `platform/web_crawl`, `NEXUS_SANDBOX=opensandbox`-backed `platform/shell`, `activate_skill`, and `platform/delegate` with the two conventions above | A reviewer can exercise every capability this task added, and the delegation/skill patterns Phases 7–8 already shipped, without typing a prompt from scratch |
| 16.10 | `evals/corpus/capability_web_crawl_round_trip.yaml` — a deterministic `provider/fake`-scripted case (task 1.8/#10, `ProviderScriptCase`) proving a `web_crawl` `tool_use`/`tool_result` round-trip concatenates content correctly and reaches a clean `stop`, the exact shape `capability_multi_tool_use.yaml` already proves for `file_read`, extended to the new tool name | The new tool's wire shape is exercised by the same deterministic corpus every other builtin's is — not a side door that skips it |

**Demo**: `make agentic-up`, then `nexusctl run "research the top 3 headlines on https://news.ycombinator.com and summarize them"` — the run calls `activate_skill("web-research")`, then `platform/web_crawl`, and returns a summary citing the crawled URL. Separately, `NEXUS_SANDBOX=opensandbox make run` and `nexusctl run --autonomy autonomous "delegate to a sandbox-runner sub-agent: run 'python3 --version' and report it"` — the transcript shows a `platform/delegate` call with `scope_grant:["platform/shell@v1",...]`, the child session's `platform/shell` call routed through `internal/sandbox/opensandbox.go`, and the same audit/taint/cost accounting every other tool call already gets. In the web app, the suggested-prompt chips reproduce both without typing.
**Acceptance**: 16.2 (a table-driven test asserting `WebCrawl{}.Taint()` matches `WebFetch{}.Taint()` field-for-field); 16.4 (`NEXUS_SANDBOX=opensandbox` with the compose service down logs a warning and `platform/shell` still runs unsandboxed — the same fallback contract `docker` already has, never a hard failure); 16.10 green in `make eval` with zero other regressions.

---

## 3. Effort summary

| Phase | Days | Cumulative | Ships |
|---|---|---|---|
| 0 · Setup | 1 | 1 | — |
| 1 · Foundational seams | 6 | 7 | Schema you never have to migrate |
| 2 · Kernel loop | 5 | 12 | **A working agent over REST** |
| 3 · Pipeline + permission chain + hooks | 8 | 20 | A *governed* agent |
| 4 · Cost governance | 5 | 25 | An agent that cannot run away |
| 5 · Trust surface | 9 | 34 | An agent a security review survives |
| 6 · Reliability | 6 | 40 | An agent that survives `kill -9` |
| 7 · Memory, skills, surfaces | 6 | 46 | An agent that grows and multiplies surfaces |
| 8 · Orchestration + delegation | 7 | 53 | Processes, not just conversations |
| 9 · Peer agent teams | 7 | 60 | Peers, not just a tree |
| 10 · Eval gate + go-live | 5 | 65 | An agent you can safely **change** |
| 11 · Additional surfaces (MCP, OAuth, Telegram/Zalo/email/cron, web) | 9 | 74 | Every surface the original names, not just the two that prove the pattern |
| 12 · Retrieval tier (pgvector) + document conversion | 6 | 80 | Durable knowledge beyond what file-first memory can carry |
| 13 · Production hardening | 18 | 98 | An agent that is actually deployable, not just architecturally sound |

≈ **98 working days** solo. Phases 2 and 3 alone (12 days after setup + seams, i.e. day 20) give
you a demonstrable, governed, single-surface agent — that is the natural first public milestone.
Phases 11–12 are additive and ship last, on a trigger (README §5), not on a fixed schedule — the
65-day core plan through Phase 10 is a complete, governed agent on its own. Phase 13 is not gated
on a trigger — it runs once, after whichever phase is current when it is scheduled, and before this
binary is exposed to anything but a developer's laptop.

## 4. Risks and how the plan absorbs them

| Risk | Mitigation built into the plan |
|---|---|
| **Phase 3 is huge and blocks everything downstream** | Split at the natural seam: 3.1–3.5 (pipeline) can ship and be tested before 3.6–3.11 (chain + hooks). The pipeline with a stub chain is still a shippable increment. |
| **The permission chain becomes untestable combinatorics** | It is a *total order* of 10 layers with ≤4 outcomes each — a table-driven test enumerating layer × outcome is ~200 rows, and that exhaustiveness is what makes an undefined interaction impossible. |
| **RLS + PgBouncer setup eats days** | Do it on day 1 of Phase 1 and let 1.4 be the phase's gate. Discovering the transaction-local rule in Phase 6 is the expensive version. |
| **The eval gate feels premature in Phase 1** | Ship it with 5 cases and code graders only; grow to 20 with the judge in Phase 10. The point is that the harness and the CI wiring exist before behavior does. |
| **A shared task board reopens the taint-laundering hole delegation just closed** | It doesn't get its own rules: 9.6 is the same fold-on-boundary-crossing idea as 8.11, just triggered by a read instead of a return, and 9.7 requires the same injection scan (7.1) before a card is ever surfaced. No board content reaches another peer's context unscanned or untainted. |
| **Cost metering gets bolted onto foreground turns only** | 4.8 is a task, and a test asserts every `Provider.Stream` caller in the codebase passes through `BudgetGate.Reserve` — enforced by an AST check, not by review. |
| **Scope creep back toward all 191 FRs** | README §5 has a named trigger per deferral. If the trigger has not fired, the answer is no. |
| **"Simplified" quietly becomes "weakened"** | §1's fidelity column is the contract. Anything marked **F** that ships as **S** is a plan change requiring a note here — the same discipline the source constitution applies to itself. |
| **Six new surfaces (Phase 11) each grow their own bespoke auth/permission logic** | They don't get any: every new surface reuses the capability descriptor (#49) and the unmodified permission chain (#17); 11.8's conformance suite is what catches a surface that quietly special-cases itself. |
| **Retrieval (Phase 12) becomes a second, unscoped copy of tenant knowledge that erasure can't reach** | 12.8 makes this the phase's gate, the same way 1.4 gates Phase 1: erasure must empty the retrieval index in the same transaction, not on a best-effort follow-up job. |
| **A pattern rated F on paper isn't at full fidelity in the shipped adapter** | [`docs/production-readiness-review.md`](production-readiness-review.md) exists precisely to audit shipped code against §1's claims independently of this plan; Phase 13 closes what it found (F4/F5/F7/F8/F13/F15) before any new pattern work resumes. |

