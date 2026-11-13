---

description: "Task list for the Nexus Agent Platform baseline (retroactive)"
---

# Tasks: Nexus Agent Platform (baseline)

**Input**: Design documents from `/specs/001-agent-platform/`

**Prerequisites**: [plan.md](plan.md), [spec.md](spec.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/), [quickstart.md](quickstart.md)

**Tests**: Included. Tests are *requested* by this project: the constitution (Principle IX) makes the eval gate and deterministic-provider tests a
foundational deliverable, and the property test for the paired-result invariant is mandated. Test tasks name the existing test file.

**Paths**: verified at HEAD (`tests/contract/specs_test.go` fails if a path named by a `[X]` task does not exist).

**Organization**: grouped by user story. Each story maps to one or more phases of the build ledger (`docs/build-phases.md`, deleted; frozen at
`09d54f8`, see [../README.md](../README.md)); every task carries its ledger id as `(bp N.M)` and the requirements it proves.

> **Retroactive.** This list documents work already shipped at `8e02491`. `[X]` means **"every file this task names exists at `HEAD`"** — verified
> mechanically (see *Verification* at the end), **not** that the story's acceptance scenario passes. Run [quickstart.md](quickstart.md) for that. `[ ]`
> means unbuilt or not verified present. The check is now enforced: `tests/contract/specs_test.go` fails if a path named by a `[X]` task does not exist.
> Gaps found while reconstructing the artifacts are appended by `/speckit-converge` in the **Convergence backlog** at the end.

## Format: `- [X] T### [P?] [US#?] Description in path (bp ledger-id; FR/SC)`

- **[P]**: parallelizable (different files, no dependency on an incomplete task)
- **[US#]**: the user story in [spec.md](spec.md); omitted for Setup, Foundational and Polish
- **(bp N.M)**: the task id in the build ledger, which code comments cite as `task N.M` (resolved by [../README.md](../README.md)) · **(post-ledger)**: shipped after the ledger was last updated, so it has no ledger id
- Spec Kit phase numbers below are **not** the ledger's phase numbers; each heading states its ledger phase

| Spec Kit phase | Story | Ledger phase | | Spec Kit phase | Story | Ledger phase |
|---|---|---|---|---|---|---|
| 1 Setup | — | 0 | | 10 | US8 eval gate, go-live (P2) | 10 |
| 2 Foundational | — | 1 | | 11 | US9 production readiness (P2) | 13–15 |
| 3 | US1 run a task (P1) 🎯 | 2 | | 12 | US10 peer teams (P3) | 9 |
| 4 | US2 governed tools (P1) | 3 | | 13 | US11 more surfaces (P3) | 11 |
| 5 | US3 cost ceilings (P1) | 4 | | 14 | US12 retrieval (P3) | 12 |
| 6 | US4 trust surface (P2) | 5 | | 15 | US13 agentic capability (P3) | 16 |
| 7 | US5 reliability (P2) | 6 | | 16 | US14 observability (P3) | 17 |
| 8 | US6 memory, skills, surfaces (P2) | 7 | | 17 | Polish | — |
| 9 | US7 plans, delegation (P2) | 8 | | 18 | post-ledger increments | — |

---

## Phase 1: Setup (ledger Phase 0)

**Purpose**: toolchain, local infrastructure, lint, CI.

- [X] T001 Pin the Go toolchain and module path in `go.mod` (bp 0.1)
- [X] T002 [P] Define the infrastructure stack — Postgres 17 + pgvector, PgBouncer in **transaction-pooling** mode on :6432, Redis 7 with append-only persistence — in `deploy/docker-compose.yml` (bp 0.2)
- [X] T003 [P] Configure lint — `exhaustive` with `default-signifies-exhaustive: false`, `errcheck`, `gosec`, `forbidigo` banning `fmt.Print*` outside `cmd/` and the math/rand package — in `.golangci.yml` (bp 0.3)
- [X] T004 [P] Add the Makefile target set (`up`, `migrate`, `seed`, `run`, `token`, `test`, `eval`, `verify-chain`, `go-live`, …) in `Makefile` (bp 0.4)
- [X] T005 [P] CI: build → lint → govulncheck → unit (`-race -cover`) → integration (testcontainers) → eval gate in `.github/workflows/ci.yml` (bp 0.5, 14.3)
- [X] T006 [P] Copy the constitution in as the review checklist: `docs/constitution.md`, `.specify/memory/constitution.md` (bp 0.6)

---

## Phase 2: Foundational (ledger Phase 1) — blocks every story

**Purpose**: the schema and contract decisions that are expensive-to-impossible to retrofit onto a log that already has rows.

**⚠️ CRITICAL**: no story work begins until this phase is complete. **Acceptance**: T010 fails loudly if `set_config(…, true)` becomes `false`.

- [X] T007 Migrations for `tenants`, `sessions` (harness digest, fork columns, root/parent/depth, plan seams) and `events` (schema version, plaintext digest, key id, pair ref, trace/span) in `migrations/0001_tenants.sql`, `migrations/0002_sessions.sql`, `migrations/0003_events.sql` (bp 1.1; FR-001, FR-004, FR-005, FR-092)
- [X] T008 Runtime role `nexus_app` (`NOBYPASSRLS`), RLS enabled **and forced** on every tenant table, append-only trigger plus `REVOKE` on `events` in `migrations/0000_app_role.sql`, `migrations/0003_events.sql` (bp 1.2; FR-002, FR-004)
- [X] T009 Transaction-local tenant scope — the only scoping call — in `internal/store/tenant.go` (bp 1.3; FR-003)
- [X] T010 [P] Isolation test **through PgBouncer**, failing when scope is session-level, in `internal/store/isolation_integration_test.go` (bp 1.4; FR-003, SC-009)
- [X] T011 Event taxonomy and the envelope upcasting registry in `internal/eventlog/event.go`, `internal/eventlog/upcast.go` (bp 1.5; FR-005)
- [X] T012 [P] Envelope encryption — KEK → per-tenant DEK, AES-256-GCM, digest over plaintext — in `internal/crypto/envelope.go`, `internal/crypto/postgres/keystore.go` (bp 1.6; FR-007)
- [X] T013 [P] Deny-by-default telemetry attribute allowlist and filtering exporter in `internal/obs/allowlist.go`, `internal/obs/span.go` (bp 1.7; FR-009, SC-014)
- [X] T014 [P] Deterministic scripted provider (truncation, stall, malformed stream, throttle, failover) in `internal/provider/fake/fake.go`, `internal/provider/fake/load.go` (bp 1.8; FR-008)
- [X] T015 [P] Eval harness skeleton — corpus loader, runner, code graders, CI gate — in `evals/runner.go`, `evals/gate.go`, `evals/corpus/` (bp 1.9; FR-011)
- [X] T016 Harness digest over prompt version, catalog manifest, skill set, safety and approval policy, prompt mode in `internal/harness/digest.go` (bp 1.10; FR-010)
- [X] T017 Transitive import-graph boundary contract test in `tests/contract/boundaries_test.go` (bp 1.11; FR-012)
- [X] T018 Embedded, filename-ordered, expand-only migration runner in `internal/store/migrate.go`, `migrations/embed.go` (FR-013)
- [X] T019 [P] Enforce the 500-line cap on non-test Go files in `tests/contract/filesize_test.go` (post-ledger; FR-014)
- [X] T020 [P] PR-size gate (1,000 changed lines) in `scripts/pr-size.sh`, `.github/workflows/pr-size.yml` (post-ledger)
- [X] T021 [P] `.env` loading where a real environment variable always wins, with every variable documented, in `internal/dotenv/`, `.env.example` (post-ledger)

**Checkpoint**: foundation ready — stories may proceed.

---

## Phase 3: User Story 1 — Run an agent task and watch it happen (P1) 🎯 MVP (ledger Phase 2)

**Goal**: submit a task, stream ordered events to exactly one typed terminal outcome, survive a mid-turn kill.

**Independent Test**: quickstart §2 — `POST /v1/runs` → `202`; SSE ends in one `terminal`; kill mid-turn and confirm every `tool_use` still has a paired result.

### Tests for User Story 1

- [X] T022 [P] [US1] Property test: over generated histories every `tool_use` has exactly one `tool_result` before the next model call in `tests/property/paired_result_test.go` (bp 2.5; FR-018, SC-001)
- [X] T023 [P] [US1] Prefix byte-equality across turns and cache-read computed from per-class counts in `internal/promptctx/builder_test.go` (bp 2.7; FR-020, FR-021, SC-002, SC-003)
- [X] T024 [P] [US1] End-to-end REST run test in `tests/integration/rest_run_test.go` (FR-026)
- [X] T025 [P] [US1] Table-driven tests for classification and terminal reasons, every arm, no Docker, in `kernel/classify_test.go`, `kernel/terminal_test.go` (bp 14.1; F9; FR-106)

### Implementation for User Story 1

- [X] T026 [US1] The loop — an `iter.Seq2[Event, error]` generator: hygiene → reserve → stream → classify → dispatch → pair → terminate — in `kernel/loop.go`, `kernel/turns.go` (bp 2.1; FR-015)
- [X] T027 [P] [US1] Typed response classification (`TOOL_CALLS | CONTENT | EMPTY`), dispatching on classification never on text, in `kernel/classify.go` (bp 2.2; FR-016)
- [X] T028 [P] [US1] Typed terminal reasons, each with a named producer, exhaustively handled, in `kernel/terminal.go` (bp 2.3; FR-017)
- [X] T029 [P] [US1] Pre-call hygiene — drop orphan results, backfill synthetic results, prune stale observations — in `kernel/hygiene.go` (bp 2.4; FR-019)
- [X] T030 [P] [US1] Two-zone prompt builder (byte-stable prefix + volatile tail) and cache measurement in `internal/promptctx/builder.go`, `internal/promptctx/cache.go` (bp 2.6; FR-020, FR-021)
- [X] T031 [US1] Provider port with one normalized stream and usage split by token class in `internal/provider/provider.go` (bp 2.8; FR-022)
- [X] T032 [P] [US1] Anthropic adapter (HTTP, hand-rolled) in `internal/provider/anthropic/anthropic.go`, `internal/provider/anthropic/request.go`, `internal/provider/anthropic/response.go`, `internal/provider/anthropic/stream.go` (bp 2.8; FR-022)
- [X] T033 [P] [US1] Deterministic router by data label and difficulty, decision persisted on the session, in `internal/provider/router.go` (bp 2.8; FR-023)
- [X] T034 [P] [US1] Failover taxonomy; commit after the first chunk; never fail over on context overflow, in `internal/provider/failover.go` (bp 2.9; FR-024)
- [X] T035 [US1] REST surface — create (202), get, SSE events (audience-gated), list sessions — in `internal/surfaces/rest/server.go`, `internal/runs/rest/run_create.go`, `internal/runs/rest/run_get.go`, `internal/runs/rest/run_events.go`, `internal/runs/rest/sessions_list.go` (bp 2.10; FR-026)
- [X] T036 [US1] The single composition point that builds a `RunState`/`RunConfig`: `kernelRunStarter` in `cmd/nexusd/runner.go` (FR-015)
- [X] T037 [P] [US1] State rehydration from the log in `kernel/rehydrate.go` (FR-006)
- [X] T038 [P] [US1] Metered-call helpers split out of `kernel/turns.go` into `kernel/metering.go` (post-ledger, `903fe3d`; a pure move)

**Checkpoint**: US1 is a working, demonstrable agent over REST — the natural first public milestone with US2.

---

## Phase 4: User Story 2 — Governed tool use (P1) (ledger Phase 3)

**Goal**: every tool call passes one 16-step pipeline and one 10-layer permission order; safety is per-invocation and fails closed.

**Independent Test**: quickstart §3 — a destructive request is refused at `read_only` and suspends at `supervised`.

### Tests for User Story 2

- [X] T039 [P] [US2] Table-driven test over the full layer cross-product in `internal/permissions/chain_test.go` (bp 3.6; FR-035)
- [X] T040 [P] [US2] Standing scope cannot skip layers 6 or 7 — in `internal/permissions/chain_test.go` (bp 3.6; SC-005)
- [X] T041 [P] [US2] A hook returning ALLOW is treated as DEFER; out-of-allowlist rewrites are refused in `internal/hooks/dispatcher_test.go` (bp 3.11; FR-040)
- [X] T042 [P] [US2] Pipeline tests, one per step, in `internal/tools/pipeline_test.go` (bp 3.4; FR-033)
- [X] T043 [P] [US2] Permission denial and approval-suspend integration tests in `tests/integration/permission_chain_test.go` (SC-004)

### Implementation for User Story 2

- [X] T044 [P] [US2] Qualified tool identity, one owner per namespace, collision refused at admission in `internal/tools/identity.go` (bp 3.1; FR-030)
- [X] T045 [P] [US2] Catalog manifest pinning the resolvable universe into the harness digest in `internal/tools/manifest.go` (bp 3.2; FR-031)
- [X] T046 [P] [US2] Descriptor admission scan (`pending → clean | flagged | rejected`, fail closed) in `internal/tools/admit.go` (bp 3.3; FR-032)
- [X] T047 [US2] The 16-step pipeline in `internal/tools/pipeline.go`, `internal/tools/execute.go`, `internal/tools/dispatch.go`, `internal/tools/pipeline_helpers.go` (bp 3.4; FR-033)
- [X] T048 [P] [US2] Canonical digest — approval binding, idempotency key, resume re-verification — in `internal/tools/digest.go` (bp 3.5; FR-034)
- [X] T049 [US2] Ten-layer permission chain, deny rules and glob matching in `internal/permissions/chain.go`, `internal/permissions/denyrules.go`, `internal/permissions/glob.go`, `internal/permissions/types.go` (bp 3.6; FR-035)
- [X] T050 [P] [US2] Autonomy as a one-way ratchet (no widening function) in `internal/permissions/autonomy.go` (bp 3.7; FR-036)
- [X] T051 [P] [US2] Tool profiles (Gate 1) as versioned tenant config in `internal/permissions/profile.go` (bp 3.8; FR-037)
- [X] T052 [P] [US2] Hybrid safety classifier — rules, then a bounded model leg with circuit breaker, fail closed to ASK — in `internal/permissions/safety/safety.go` (bp 3.9; FR-038)
- [X] T053 [P] [US2] Rule of Two over declared taint legs, with `Rebaseline` clearing only the untrusted leg, in `internal/permissions/ruleoftwo.go` (bp 3.10; FR-039)
- [X] T054 [US2] Hook layer — command / http (SSRF-guarded) / prompt handlers, matcher, JSON-AST `if`, bounded chain — in `internal/hooks/dispatcher.go`, `internal/hooks/exechook/handler.go`, `internal/hooks/httphook/handler.go`, `internal/hooks/prompt.go`, `internal/hooks/matcher.go`, `internal/hooks/types.go` (bp 3.11; FR-040)
- [X] T055 [P] [US2] Builtin tools with taint and effect-class declarations in `internal/tools/builtin/file_read.go`, `internal/tools/builtin/file_write.go`, `internal/tools/builtin/file_search.go`, `internal/tools/builtin/shell.go`, `internal/tools/builtin/web_fetch.go`, `internal/tools/builtin/workspace.go` (bp 3.12; FR-041)
- [X] T056 [P] [US2] Result budgeting — cap/paginate ~25k tokens, spill to the blob dir, preview with a "do not infer success" banner — in `internal/tools/budget.go` (bp 3.13; FR-042)
- [X] T057 [P] [US2] Tool-execution adapter between the kernel and the pipeline in `internal/tools/kerneladapter/executor.go` (FR-033)
- [X] T058 [P] [US2] `platform/ask_clarification` — always suspends for a human regardless of autonomy, via Gate 2 forcing ASK — in `internal/tools/builtin/ask_clarification.go` (post-ledger; chat UI)

**Checkpoint**: US1 + US2 = a governed single-surface agent.

---

## Phase 5: User Story 3 — Costs that cannot run away (P1) (ledger Phase 4)

**Goal**: reserve worst-case cost before every model call; stop on cost before the overspending call.

**Independent Test**: quickstart §4 — `--budget=0.05` ends `cost_exhausted` first; 20 concurrent sessions never exceed one tenant ceiling.

### Tests for User Story 3

- [X] T059 [P] [US3] 20 concurrent sessions respect one tenant ceiling; unreported usage reconciles at worst case in `tests/integration/cost_ceiling_test.go` (bp 4 acceptance; SC-006)
- [X] T060 [P] [US3] AST test: every provider call passes the budget gate in `tests/contract/cost_metering_test.go` (bp 4.8; FR-050, SC-032)
- [X] T061 [P] [US3] No float in money, enforced by test in `internal/cost/money_notfloat_test.go` (bp 4.1; FR-043)

### Implementation for User Story 3

- [X] T062 [P] [US3] Exact-integer `Money` with explicit currency, rounded once, in `internal/cost/money.go` (bp 4.1; FR-043)
- [X] T063 [P] [US3] Meter registry with a reservable flag in `internal/cost/meter.go` (bp 4.2; FR-044)
- [X] T064 [P] [US3] Versioned price book keyed `(meter, subject, effective range)` in `internal/cost/pricebook.go` (bp 4.3; FR-045)
- [X] T065 [US3] Reserve → stream → reconcile gate over an atomic Redis counter with an epoch marker in `internal/cost/gate.go`, `internal/cost/gate_helpers.go`, `internal/cost/reserve.go`, `internal/cost/reconcile.go`, `internal/cost/redis/counters.go` (bp 4.4; FR-046)
- [X] T066 [P] [US3] Worker-local synchronous per-run ceiling in `internal/cost/budget.go` (bp 4.5; FR-047)
- [X] T067 [P] [US3] `budget_decision` for every resolution — allow, refuse, degrade, skip — in `internal/cost/decision.go` (bp 4.6; FR-048)
- [X] T068 [P] [US3] UNREPORTED reconcile at the full reserved worst case, flagged, in `internal/cost/reconcile.go` (bp 4.7; FR-049)
- [X] T069 [US3] Route compaction, safety leg, prompt hooks, judge and titles through the gate in `kernel/metering.go`, `internal/promptctx/condense.go` (bp 4.8; FR-050)
- [X] T070 [P] [US3] Cost records appended in the turn's transaction and shipped through the outbox in `internal/cost/record.go` (bp 4.9; FR-051)

**Checkpoint**: US1–US3 = an agent that is governed *and* bounded.

---

## Phase 6: User Story 4 — A trust surface a security review survives (P2) (ledger Phase 5)

**Goal**: attributable, tamper-evident, erasable; high-impact actions suspend on a digest-bound human decision.

**Independent Test**: quickstart §5.

### Tests for User Story 4

- [X] T071 [P] [US4] Erasure: replay still works structurally and the chain still verifies in `tests/integration/oversight_test.go` (bp 5.5; FR-054, SC-010)
- [X] T072 [P] [US4] Approval transaction: modified grant, mismatch, simulated consent in `tests/integration/oversight_test.go` (bp 5.6, 5.7, 5.14; FR-056, SC-012, SC-025)
- [X] T073 [P] [US4] Adversarial oversight eval cases in `evals/adversarial_test.go`, `evals/corpus_safety.go` (bp 5.14; FR-063, SC-025)

### Implementation for User Story 4

- [X] T074 [US4] Sign-only key custodian over a unix socket in `cmd/signerd/main.go`, `internal/audit/protocol.go`, `internal/audit/signer.go`, `internal/audit/signerkey/` (bp 5.1; FR-052)
- [X] T075 [US4] Per-session hash-chained receipts over digests in `internal/audit/chain.go` (bp 5.2; FR-052)
- [X] T076 [P] [US4] Periodic anchoring and a scheduled verifier in `internal/audit/anchor.go`, `internal/audit/verify.go`, `cmd/nexusd/background.go` (bp 5.3; FR-053)
- [X] T077 [US4] Crypto-shredding erasure — destroy the DEK, hard-delete derived artifacts in one transaction, reconciliation job — in `internal/crypto/postgres/shred.go`, `migrations/0008_derived_artifacts.sql`, `cmd/nexusd/cli_erase.go` (bp 5.4; FR-054)
- [X] T078 [US4] Approval transaction: digest binding, decision-ready context package, assignee, TTL, five outcomes in `internal/oversight/approval.go`, `internal/oversight/types.go`, `migrations/0007_oversight.sql` (bp 5.6; FR-055)
- [X] T079 [P] [US4] Step-9a digest re-verification yielding a typed `approval_mismatch` in `internal/tools/execute.go`, `internal/oversight/resume.go` (bp 5.7; FR-056)
- [X] T080 [US4] Durable suspend at zero token cost, resume on the approval event in `internal/oversight/resume.go`, `kernel/loop.go` (bp 5.8; FR-057)
- [X] T081 [P] [US4] Input requests — schema-declared, `on_expiry`, zero authorization — in `internal/oversight/input.go` (bp 5.9; FR-058)
- [X] T082 [P] [US4] `Invalidate` on cancel, terminal, reap, ceiling breach and steer-into-suspension in `internal/oversight/invalidate.go` (bp 5.10; FR-059)
- [X] T083 [P] [US4] Content-access grants with a receipt on grant and every read in `internal/contentaccess/grants.go`, `migrations/0009_content_access_grants.sql`, `internal/contentaccess/rest/handler.go` (bp 5.11; FR-060)
- [X] T084 [US4] Docker sandbox — `--network none`, CPU/memory/PID/wall limits, per-session workspace, breach → terminate and reclaim — in `internal/sandbox/sandbox.go`, `internal/sandbox/session.go`, `migrations/0010_sandbox.sql` (bp 5.12; FR-061)
- [X] T085 [P] [US4] Egress allowlist for `web_fetch`; connector/MCP endpoints in the sandbox deny set in `internal/tools/builtin/web_fetch.go` (bp 5.13; FR-062)
- [X] T086 [P] [US4] Approval REST endpoints in `internal/oversight/rest/handler.go` (bp 5.6; FR-055)

**Checkpoint**: an agent a security review survives — with the caveats recorded in spec *Known deviations* 5.

---

## Phase 7: User Story 5 — A run that survives failure (P2) (ledger Phase 6 + post-ledger)

**Goal**: resume from a checkpoint after a kill; classify before retry; never re-execute an in-flight effect.

**Independent Test**: quickstart §6.

### Tests for User Story 5

- [X] T087 [P] [US5] Reliability integration tests — resume after kill, unresolved claims, stuck detection, snapshot deletion — in `tests/integration/reliability_test.go` (bp 6.4, 6.6; FR-066, FR-068, FR-070, SC-015, SC-016)
- [X] T088 [P] [US5] Queue tests — reclaim abandoned lease, no double lease, serial-per-session lock — in `internal/queue/worker_test.go`, `tests/integration/reliability_test.go` (bp 6.1, 6.2; FR-064)
- [X] T089 [P] [US5] Unit tests for the previously untested packages in `internal/oversight/`, `internal/audit/chain_test.go`, `internal/runctl/resume_test.go` (bp 14.1; F9; FR-106)

### Implementation for User Story 5

- [X] T090 [US5] Queue port and admission, session-key serial lock in `internal/queue/types.go`, `internal/queue/admission.go`, `internal/queue/redis/lock.go`, `internal/queue/worker.go` (bp 6.1, 6.2; FR-064)
- [X] T091 [P] [US5] Redis Streams consumer-group adapter with `XAUTOCLAIM` reclaim, replacing the Postgres adapter, in `internal/queue/redis/streams.go`, `migrations/0024_retire_queue_jobs.sql` (post-ledger, `2efa4ff`; research R-10)
- [X] T092 [P] [US5] Checkpoint artifact in `internal/runctl/postgres/checkpoint.go`, `migrations/0013_checkpoints.sql` (bp 6.3; FR-065)
- [X] T093 [P] [US5] Disposable snapshot in `internal/eventlog/postgres/snapshots.go` (bp 6.4; FR-066)
- [X] T094 [P] [US5] Condensation as a model-facing event in `internal/eventlog/condensation.go` (bp 6.5; FR-067)
- [X] T095 [US5] Write-ahead effect claims in `internal/runctl/postgres/claims.go`, `internal/runctl/claims.go`, `migrations/0012_claims.sql` (bp 6.6; FR-068)
- [X] T096 [P] [US5] Failure classifier, jittered logged backoff, circuit break at three in `internal/reliability/classifier.go` (bp 6.7; FR-069)
- [X] T097 [P] [US5] Stuck detection — `stuck_suspected` then terminate on a corroborating second trip — in `internal/reliability/stuck.go` (bp 6.8; FR-070)
- [X] T098 [US5] Run control — steer, cancel (sole producer of `aborted`), resume, tighten autonomy — in `internal/runctl/steer.go`, `internal/runctl/cancel.go`, `internal/runctl/resume.go`, `internal/runctl/autonomy.go`, `internal/runctl/types.go`, `internal/eventlog/sealed/unit.go` (bp 6.9; FR-071)
- [X] T099 [P] [US5] Pure replay in `internal/runctl/replay.go` (bp 6.10; FR-072)
- [X] T100 [P] [US5] Fork — effects disabled, no inherited approvals, own budget/audit chain, digest divergence — in `internal/runctl/fork.go` (bp 6.11; FR-072)
- [X] T101 [P] [US5] Run-control REST endpoints in `internal/runctl/rest/handler.go` (bp 6.9; FR-071)
- [X] T102 [P] [US5] Conversational sessions — `awaiting_input` pause, in-place resume, idle sweep to `idle_timeout` — in `internal/runctl/converse.go`, `migrations/0023_conversational_sessions.sql`, `cmd/nexusd/background.go` (post-ledger, `2975d08`; FR-029)
- [X] T103 [P] [US5] Durable Rule-of-Two taint — `taint_transition` appended after `tool_result`, restored on every resume path — in `kernel/rehydrate.go`, `internal/tools/pipeline.go` (post-ledger, `08a86b7`; FR-039)
- [X] T104 [P] [US5] Per-peer session-key lookup for resuming conversations in `internal/store/session.go` (post-ledger, `7606e15`; FR-121)

**Checkpoint**: an agent that survives `kill -9`.

---

## Phase 8: User Story 6 — Memory, skills, surfaces (P2) (ledger Phase 7)

**Goal**: durable memory and vetted skills that never widen capability; the CLI proves a second surface needs no kernel change.

**Independent Test**: quickstart §7.

### Tests for User Story 6

- [X] T105 [P] [US6] Memory injected at start with a leading audit event; mid-session skill revocation emits `skill_capability_ignored` in `tests/integration/harness_growth_test.go` (bp 7.1, 7.8; FR-073, FR-076, SC-019)
- [X] T106 [P] [US6] REST and CLI produce identical event sequences and terminal reasons in `tests/integration/harness_growth_test.go` (bp 7.15; FR-082, SC-018)
- [X] T107 [P] [US6] Outbox idempotency and `failed_permanent` in `tests/integration/harness_growth_test.go` (bp 7.14; FR-081, SC-020)

### Implementation for User Story 6

- [X] T108 [P] [US6] File-first memory with injection screening and retention in `internal/memory/store.go`, `internal/memory/load.go`, `internal/memory/screen.go` (bp 7.1; FR-073)
- [X] T109 [P] [US6] Ordered, metered, degrade-capable memory consolidation in `internal/memory/consolidate.go` (bp 7.2; FR-074)
- [X] T110 [US6] Signed content-addressed skill bundles, per-file scan, three disclosure tiers in `internal/skills/types.go`, `internal/skills/loader.go`, `internal/skills/digest.go`, `internal/skills/sign.go`, `internal/skills/admit.go` (bp 7.3; FR-075)
- [X] T111 [P] [US6] `declared_tool_ids` intersect the resolved catalog; `skill_capability_ignored` in `internal/skills/catalog.go`, `internal/runctl/skills.go` (bp 7.4, 7.8; FR-076)
- [X] T112 [P] [US6] A bundled script registers as a real tool or the bundle is refused in `internal/skills/script_tool.go` (bp 7.5; FR-075)
- [X] T113 [P] [US6] Tier-1 resident manifest folded into the skill-set digest in `internal/skills/manifest.go` (bp 7.6; FR-075)
- [X] T114 [P] [US6] `activate_skill` and `read_skill_file` as ordinary tools in `internal/tools/builtin/skill.go` (bp 7.7, 7.9; FR-077)
- [X] T115 [P] [US6] Non-destructive live pruning in `internal/promptctx/prune.go` (bp 7.10; FR-078)
- [X] T116 [P] [US6] Structured compaction on a cheaper model through the same port, metered, in `internal/promptctx/condense.go` (bp 7.11; FR-078)
- [X] T117 [US6] Capability descriptors and conformance in `internal/surfaces/capability/capability.go`, `internal/surfaces/capability/render.go`, `internal/surfaces/rest/capability.go` (bp 7.12; FR-079)
- [X] T118 [P] [US6] Per-turn principal resolution in `internal/surfaces/rest/capability.go` (bp 7.13; FR-080)
- [X] T119 [US6] Delivery outbox — event appended before the send, at-least-once, idempotent — in `internal/surfaces/outbox.go`, `internal/surfaces/postgres/delivery_sql.go`, `migrations/0015_deliveries.sql` (bp 7.14; FR-081)
- [X] T120 [P] [US6] `nexusctl` as a second real surface in `cmd/nexusctl/main.go`, `internal/surfaces/cli/cli.go` (bp 7.15; FR-082)
- [X] T121 [P] [US6] Tenant configuration (admitted skills, retention) in `internal/config/config.go`, `migrations/0014_tenant_configs.sql` (bp 7; FR-073)

**Checkpoint**: `git diff kernel/` for the whole phase is empty.

---

## Phase 9: User Story 7 — Plans and delegation (P2) (ledger Phase 8)

**Goal**: deterministic, zero-token routing; delegation whose scope descends, taint ascends and bounds fail closed.

**Independent Test**: quickstart §8.

### Tests for User Story 7

- [X] T122 [P] [US7] Zero-token routing; delegation round trip; bounds fail closed through the real chain in `tests/integration/orchestration_test.go` (bp 8.6, 8.12; FR-084, FR-089, SC-022, SC-023)
- [X] T123 [P] [US7] Plan eval-gate lifecycle in `tests/integration/orchestration_test.go` (bp 8.4; FR-086)
- [X] T124 [P] [US7] Delegation unit tests in `internal/delegate/delegations_test.go`

### Implementation for User Story 7

- [X] T125 [P] [US7] Plan schema — seven step kinds, transitions, cost envelope, allowed tools — in `internal/plan/schema.go`, `internal/plan/steps.go` (bp 8.1; FR-083)
- [X] T126 [P] [US7] Closed JSON-AST predicate (`eq|ne|lt|gt|and|or|in`) in `internal/plan/predicate.go` (bp 8.2; FR-084)
- [X] T127 [US7] Validation — schema, reachability, bounded loops, closed predicates, scope subset, structural oversight completeness — in `internal/plan/validate.go` (bp 8.3; FR-085)
- [X] T128 [US7] Lifecycle `draft → validated → eval_passed → signed_off → enabled`, pinned and immutable, in `internal/plan/lifecycle.go`, `migrations/0016_orchestration.sql` (bp 8.4; FR-086)
- [X] T129 [US7] Platform-evaluated execution with checkpointed step boundaries and plan events in `internal/plan/exec.go`, `internal/plan/run.go`, `internal/plan/ports.go`, `internal/plan/replay.go`, `internal/plan/rehydrate.go` (bp 8.5; FR-087)
- [X] T130 [P] [US7] `preauth` — enumerated, digest-bound — in `internal/plan/steps.go` (bp 8.7; FR-088)
- [X] T131 [US7] Delegation as a tool through the same pipeline in `internal/delegate/spawn.go`, `internal/delegate/resolve.go`, `internal/delegate/delegations.go`, `internal/delegate/postgres/store.go`, `internal/delegate/types.go` (bp 8.8; FR-089)
- [X] T132 [P] [US7] `platform/delegate` with scope re-derived as a provable subset in `internal/delegate/tool/delegate.go` (bp 8.9; FR-089)
- [X] T133 [P] [US7] Delegation suspends on the same durable-suspend primitive as approvals in `kernel/loop.go`, `internal/delegate/resolve.go` (bp 8.10; FR-090)
- [X] T134 [P] [US7] Taint ascend from the child's own event-derived state in `internal/delegate/resolve.go` (bp 8.11; FR-089, SC-021)
- [X] T135 [P] [US7] Fan-out envelope reserved before the first child; reaping; return-schema validation in `internal/delegate/envelope.go`, `migrations/0016_orchestration.sql` (bp 8.13, 8.14; FR-091)
- [ ] T136 [P] [US7] **Deferred stretch** — in-sandbox broker as the only route from sandbox code into the pipeline; `internal/sandbox/broker.go` does not exist (bp 8.15; FR-062). Connectors stay in the egress deny set until it ships

**Checkpoint**: processes, not just conversations.

---

## Phase 10: User Story 8 — Eval gate and go-live (P2) (ledger Phase 10)

**Goal**: say whether a change is safe to ship using trial statistics; verify a deployment against a go-live checklist.

**Independent Test**: quickstart §9.

### Tests for User Story 8

- [X] T137 [P] [US8] Gate, regression and statistics tests in `evals/gate_test.go`, `evals/regression_test.go`, `evals/stats_test.go`, `evals/grading_test.go`, `evals/policy_test.go` (bp 10.2; FR-094, SC-026)
- [X] T138 [P] [US8] Eval-gate and golden-signal integration tests in `tests/integration/evalgate_test.go`, `tests/integration/reporting_dashboard_test.go` (bp 10.12; FR-100)

### Implementation for User Story 8

- [X] T139 [P] [US8] Corpus split into regression / capability / safety / negative with distinct thresholds in `evals/corpus/`, `evals/policy.go` (bp 10.1; FR-093)
- [X] T140 [US8] k trials, Wilson intervals, interval-separation regression, three-valued verdict in `evals/stats.go`, `evals/gate.go`, `evals/baseline.go` (bp 10.2; FR-094)
- [X] T141 [P] [US8] Eval-environment digest; refuse cross-digest comparison; cold sandboxes in `evals/environment.go` (bp 10.3; FR-095)
- [X] T142 [P] [US8] Deterministic graders first; pinned cross-family calibrated judge in `evals/judge.go` (bp 10.4, 10.5; FR-096)
- [X] T143 [P] [US8] Held-out graders and a measured visible-vs-held-out gap in `evals/heldout.go`, `evals/corpus/heldout/` (bp 10.6; FR-096)
- [X] T144 [P] [US8] Trajectory grading in `evals/trajectory.go` (bp 10.7; FR-097)
- [X] T145 [P] [US8] Efficiency bands gated alongside quality in `evals/efficiency.go` (bp 10.8; FR-097, SC-026)
- [X] T146 [P] [US8] Mandatory HITL adversarial cases in `evals/permission_case.go`, `evals/corpus_safety.go` (bp 10.9; FR-063)
- [X] T147 [P] [US8] Per-artifact case sets (skill, tool, plan, team) with an adapter to the plan lifecycle in `evals/artifact.go`, `internal/evalgate/adapter.go` (bp 10.10; FR-098)
- [X] T148 [P] [US8] CI gate: ≥ 90% pass and zero regressions blocks merge in `.github/workflows/ci.yml`, `evals/cmd/runner/main.go`, `evals/testdata/baseline.json` (bp 10.11; FR-099, SC-027)
- [X] T149 [P] [US8] Golden-signal computation and dashboard in `internal/reporting/golden_signals.go`, `cmd/nexusd/cli_dashboard.go` (bp 10.12; FR-100)
- [X] T150 [P] [US8] Go-live checklist with a verifying command in `docs/go-live.md`, `cmd/nexusd/cli_golive.go` (bp 10.13; FR-101)

**Checkpoint**: an agent you can safely change.

---

## Phase 11: User Story 9 — Safe to put in front of real traffic (P2) (ledger Phases 13–15)

**Goal**: authenticated principals, clean lifecycle, readiness, fail-closed config, packaging, measured limits (findings F1–F15).

**Independent Test**: quickstart §10.

### Tests for User Story 9

- [X] T151 [P] [US9] Authn middleware tests in `internal/surfaces/rest/authn_test.go` (bp 13.1; F1, SC-028)
- [X] T152 [P] [US9] Config validation tests in `internal/config/config_test.go` (bp 14.4; F12, SC-028)
- [X] T153 [P] [US9] Load test publishing a measured `POST /v1/runs` rate in `tests/integration/load_bench_test.go` (bp 15.3; F14, SC-034)
- [X] T154 [P] [US9] Boundary rules for `internal/controlplane` now active in `tests/contract/boundaries_test.go` (bp 15.4; F15)

### Implementation for User Story 9 — production exposure and wire correctness (ledger Phase 13)

- [X] T155 [P] [US9] Signed-JWT per-tenant principal; delete the header-reading path in `internal/authn/authn.go`, `internal/surfaces/rest/authn.go`, `cmd/nexusd/cli_token.go` (bp 13.1; F1; FR-027)
- [X] T156 [P] [US9] Server timeouts, signal-driven graceful shutdown, `/healthz` vs `/readyz` in `cmd/nexusd/http.go`, `cmd/nexusd/serve.go` (bp 13.2; F2; FR-102)
- [X] T157 [P] [US9] Multi-stage distroless non-root images for `nexusd` and `signerd` in `Dockerfile`; compose profile in `deploy/docker-compose.yml` (bp 13.3; F3; FR-103)
- [X] T158 [US9] Typed content blocks with `tool_use_id` end to end in `internal/provider/provider.go`, `internal/provider/anthropic/request.go` (bp 13.4; F5; FR-022)
- [X] T159 [P] [US9] Cache breakpoint closing the stable zone; adaptive thinking in `internal/provider/anthropic/request.go` (bp 13.5, 13.7; F4, F7; FR-021, FR-025)
- [X] T160 [P] [US9] Per-model price-book seed with a distinct cache-read rate in `cmd/nexusd/cli_seed.go`, `internal/cost/pricebook.go` (bp 13.6; F8; FR-045)
- [X] T161 [P] [US9] Provider refusal → `refused` terminal reason with its category in `kernel/terminal.go`, `internal/provider/provider.go` (bp 13.7; F7; FR-025)

### Implementation for User Story 9 — safe to change (ledger Phase 14)

- [X] T162 [P] [US9] Live-model eval track behind `-tags=liveeval` in `evals/live_case.go`, `evals/live_corpus.go`, `evals/cmd/runner/live_liveeval.go` (bp 14.2; F6; FR-107)
- [X] T163 [P] [US9] `govulncheck` job and Dependabot in `.github/workflows/ci.yml`, `.github/dependabot.yml` (bp 14.3; F11; FR-105)
- [X] T164 [P] [US9] Validated `Config` read once, fail closed outside `--dev`, no silent KEK in `cmd/nexusd/config.go`, `cmd/nexusd/keys.go` (bp 14.4; F12; FR-104)

### Implementation for User Story 9 — operable and defensible (ledger Phase 15)

- [X] T165 [P] [US9] OTLP exporter behind `obs.Exporter`; `/metrics` in `internal/obs/otlp/exporter.go`, `cmd/nexusd/serve.go` (bp 15.1; F13; FR-108)
- [X] T166 [P] [US9] Sandbox hardening — `CapDrop ALL`, read-only root, `no-new-privileges`, non-root user — in `internal/sandbox/sandbox.go` (bp 15.2; F10; FR-061)
- [X] T167 [P] [US9] Explicit pgx pool sizing (max 20 / min 2) reconciled with the pooler in `cmd/nexusd/serve.go`; the provider SSE reader's scanner buffer raised past the 64 KB default (to 1 MiB) in `internal/provider/anthropic/stream.go`, `internal/provider/litellm/litellm.go` (bp 15.3; F14; FR-109)
- [X] T168 [US9] Versioned control-plane contract and `LocalPort` in `internal/controlplane/port.go`, `internal/controlplane/local.go`, `cmd/nexusd/controlplane.go` (bp 15.4; F15; FR-110)

**Checkpoint**: an agent that is deployable, not merely architecturally sound.

---

## Phase 12: User Story 10 — Peer agent teams (P3) (ledger Phase 9)

**Goal**: a fixed roster claims cards from a shared board without double-claiming, under one budget.

**Independent Test**: quickstart §11.

### Tests for User Story 10

- [X] T169 [P] [US10] Claim contention (exactly one winner), read-time taint fold, lifecycle, nested-team denial in `tests/integration/teams_test.go` (bp 9.4, 9.6, 9.10; FR-113, FR-114, SC-029)

### Implementation for User Story 10

- [X] T170 [US10] Team service, fixed roster, lifecycle and backstop in `internal/teams/service.go`, `internal/teams/lifecycle.go`, `internal/teams/types.go`, `internal/teams/postgres/store.go`, `migrations/0017_teams.sql` (bp 9.1, 9.2, 9.9; FR-111, FR-116)
- [X] T171 [P] [US10] Board cards, status machine, taint copy-at-creation in `internal/teams/board.go`, `migrations/0017_teams.sql` (bp 9.3; FR-112)
- [X] T172 [P] [US10] Atomic claim via `SELECT … FOR UPDATE SKIP LOCKED` in `internal/teams/board.go` (bp 9.4; FR-113)
- [X] T173 [P] [US10] Four board tools through the ordinary pipeline in `internal/teams/tool/board.go` (bp 9.5; FR-112)
- [X] T174 [P] [US10] Read-time taint fold and injection scan before a card is surfaced in `internal/teams/board.go` (bp 9.6, 9.7; FR-112, FR-114)
- [X] T175 [P] [US10] Shared envelope reserved once at creation in `internal/teams/envelope.go` (bp 9.8; FR-115)
- [X] T176 [P] [US10] Team events in `internal/teams/types.go` (bp 9.9; FR-116)

**Checkpoint**: peers, not just a tree.

---

## Phase 13: User Story 11 — More reach: MCP, connectors, messaging, scheduling, web (P3) (ledger Phase 11 + post-ledger)

**Goal**: the same task from many surfaces, with remote tools and per-user credentials admitted as strictly as builtins.

**Independent Test**: quickstart §11.

### Tests for User Story 11

- [X] T177 [P] [US11] A live OAuth token never appears in any event, log or span in `tests/integration/connectors_secret_leak_test.go` (bp 11 acceptance; FR-118, SC-030)
- [X] T178 [P] [US11] Per-surface capability conformance in `internal/surfaces/rest/capability_test.go` (bp 11.8; FR-124)

### Implementation for User Story 11

- [X] T179 [P] [US11] MCP client adapter — tools qualified `mcp/{server}/{tool}@{ver}`, per-tenant per-user resolution — in `internal/surfaces/mcp/adapter.go`, `internal/surfaces/mcp/client.go`, `internal/surfaces/mcp/resolver.go`, `internal/surfaces/mcp/port.go`, `internal/surfaces/mcp/descriptor.go`, `migrations/0019_mcp_servers.sql` (bp 11.1; FR-117)
- [X] T180 [P] [US11] Per-user OAuth vault sealed under the tenant key in `internal/connectors/oauth.go`, `migrations/0018_oauth_connections.sql` (bp 11.2; FR-118)
- [X] T181 [P] [US11] Connector tool with per-action taint in `internal/tools/builtin/connector_fetch.go` (bp 11.3; FR-119)
- [X] T182 [P] [US11] Telegram webhook surface in `internal/surfaces/telegram/webhook.go`, `internal/surfaces/telegram/sender.go`, `internal/surfaces/telegram/descriptor.go`, `internal/surfaces/telegram/ratelimit.go` (bp 11.4; FR-120)
- [X] T183 [P] [US11] Zalo webhook surface in `internal/surfaces/zalo/webhook.go`, `internal/surfaces/zalo/sender.go`, `internal/surfaces/zalo/descriptor.go`, `internal/surfaces/zalo/ratelimit.go` (bp 11.4; FR-120)
- [X] T184 [P] [US11] Email webhook and SMTP surface in `internal/surfaces/email/webhook.go`, `internal/surfaces/email/sender.go`, `internal/surfaces/email/descriptor.go`, `internal/surfaces/email/ratelimit.go` (bp 11.5; FR-120)
- [X] T185 [P] [US11] Messaging channel store in `migrations/0021_messaging_channels.sql`, `internal/surfaces/postgres/channels.go` (bp 11.4, 11.5; FR-121)
- [X] T186 [P] [US11] Cron scheduler surface with a synthetic scheduler principal in `internal/surfaces/cron/scheduler.go`, `internal/surfaces/cron/descriptor.go`, `migrations/0020_cron_schedules.sql` (bp 11.6; FR-122)
- [X] T187 [US11] Web app descriptor and chat-style UI in `internal/surfaces/web/descriptor.go`, `web/src/features/chat/ChatPage.tsx`, `web/src/features/chat/ChatThread.tsx`, `web/src/features/approvals/ApprovalCard.tsx`, `web/src/features/chat/SubAgentCard.tsx`, `web/src/features/chat/ToolCallCard.tsx`, `web/src/features/approvals/Approvals.tsx`, `web/src/features/approvals/ApprovalDetail.tsx` (bp 11.7; FR-123)
- [X] T188 [P] [US11] Surface wiring in the composition root in `cmd/nexusd/surfaces_wiring.go` (bp 11.9; FR-124)
- [X] T189 [P] [US11] Messaging session continuity — deterministic per-peer key, resume `awaiting_input`, deliver the model's reply — in `internal/surfaces/telegram/webhook.go`, `internal/surfaces/zalo/webhook.go`, `internal/surfaces/email/webhook.go` (post-ledger, `7606e15`; FR-121)

**Checkpoint**: every surface the original names, on the same seam.

---

## Phase 14: User Story 12 — Retrieval (P3) (ledger Phase 12)

**Goal**: ingest documents; retrieve through an ordinary governed, metered tool; erasure empties the index.

**Independent Test**: quickstart §11.

### Tests for User Story 12

- [X] T190 [P] [US12] Ingest/index/search/erase, rejected document, tenant isolation in `tests/integration/retrieval_test.go` (bp 12.8; FR-130, SC-031)
- [X] T191 [P] [US12] AST test: every embedding call is metered in `tests/contract/embedding_metering_test.go` (bp 12 acceptance; FR-128, SC-032)

### Implementation for User Story 12

- [X] T192 [P] [US12] Document conversion, digest and stable chunking in `internal/ingest/convert.go`, `internal/ingest/chunk.go`, `internal/ingest/types.go` (bp 12.1; FR-125)
- [X] T193 [P] [US12] Admission scan before indexing (fail closed) in `internal/ingest/admit.go` (bp 12.2; FR-126)
- [X] T194 [US12] Tenant-scoped pgvector index in `internal/retrieval/postgres/store.go`, `internal/retrieval/postgres/vector.go`, `internal/retrieval/retriever.go`, `migrations/0022_retrieval.sql` (bp 12.3; FR-127)
- [X] T195 [P] [US12] Embeddings through the provider port and the budget gate in `internal/provider/provider.go`, `internal/retrieval/retriever.go` (bp 12.4; FR-128)
- [X] T196 [P] [US12] Deterministic fake embedder in `internal/provider/fake/embedder.go` (bp 12.5; FR-128)
- [X] T197 [P] [US12] `platform/retrieve` as an ordinary tool declaring `reads_private_data` in `internal/retrieval/tool/retrieve.go` (bp 12.6; FR-129)
- [X] T198 [P] [US12] Screen retrieved chunks before they enter the prompt in `internal/retrieval/retriever.go` (bp 12.7; FR-129)
- [X] T199 [P] [US12] Extend erasure to the retrieval index, same transaction in `internal/crypto/postgres/shred.go` (bp 12.8; FR-130)
- [X] T200 [P] [US12] Ingest CLI in `cmd/nexusd/cli_ingest.go` (bp 12)

**Checkpoint**: durable knowledge beyond file-first memory.

---

## Phase 15: User Story 13 — Real capability behind the seams (P3) (ledger Phase 16)

**Goal**: a crawler and a stronger isolation backend, opt-in and configuration-selected, with every control unchanged.

**Independent Test**: quickstart §11.

### Tests for User Story 13

- [X] T201 [P] [US13] `WebCrawl{}.Taint()` equals `WebFetch{}.Taint()` field for field in `internal/tools/builtin/web_crawl_test.go` (bp 16.2; FR-131)
- [X] T202 [P] [US13] Deterministic `web_crawl` round-trip eval case in `evals/corpus/capability_web_crawl_round_trip.yaml` (bp 16.10; FR-131)

### Implementation for User Story 13

- [X] T203 [P] [US13] `platform/web_crawl` — rendered Markdown, same egress allowlist, same result budget — in `internal/tools/builtin/web_crawl.go` (bp 16.1, 16.2; FR-131)
- [X] T204 [P] [US13] OpenSandbox backend satisfying `tools.SandboxExec` in `internal/sandbox/opensandbox.go` (bp 16.3; FR-132)
- [X] T205 [US13] `NEXUS_SANDBOX=docker|opensandbox` selection with warn-and-fall-back in `cmd/nexusd/ports.go`, `cmd/nexusd/pipeline.go` (bp 16.4; FR-132)
- [X] T206 [P] [US13] Opt-in compose file for Crawl4AI and OpenSandbox in `deploy/docker-compose.agentic.yml` (bp 16.5; FR-133)
- [X] T207 [P] [US13] `agentic-up` / `agentic-down` and the capabilities doc in `Makefile`, `docs/agentic-capabilities.md` (bp 16.6; FR-133)
- [X] T208 [P] [US13] Two real skill bundles in `skills/web-research/GUIDE.md`, `skills/sandboxed-code/skill.json`, `skills/sandboxed-code/GUIDE.md`, `cmd/nexusd/cli_seed.go` (bp 16.7; FR-134) — **ledger path drift**: the ledger said `.dev/skills/`; the bundles live in `skills/`
- [X] T209 [P] [US13] Suggested-prompt chips for crawl, sandboxed shell, skill activation, delegation in `web/src/features/chat/suggestedPrompts.ts` (bp 16.9; FR-134)
- [X] T254 [P] [US13] Two named `agent_id`/`scope_grant` conventions, `web-researcher` and `sandbox-runner`, over the unmodified `platform/delegate` tool, documented in `docs/agentic-capabilities.md` and `skills/web-research/GUIDE.md` (bp 16.8; FR-134) — added 2026-11-02: the baseline missed this ledger row

**Checkpoint**: seams proven by two real third-party backends.

---

## Phase 16: User Story 14 — See what the platform is doing (P3) (ledger Phase 17 + post-ledger)

**Goal**: dashboards, alerts, logs, container metrics, traces and profiles from an opt-in stack that costs nothing when stopped.

**Independent Test**: quickstart §11.

### Tests for User Story 14

- [X] T210 [P] [US14] Tracer tree and content opt-in tests in `tests/integration/tracer_test.go` (post-ledger; FR-138)
- [X] T211 [P] [US14] Langfuse exporter and multi-destination fan-out tests in `internal/obs/langfuse/exporter_test.go`, `internal/obs/multi_test.go` (post-ledger; FR-138)

### Implementation for User Story 14

- [X] T212 [P] [US14] Structured logging with a level floor and optional file sink in `internal/obs/logging.go` (bp 17.1, 17.2; FR-135)
- [X] T213 [P] [US14] Observability compose stack — Prometheus, Alertmanager, cAdvisor, Loki, Promtail, Grafana — in `deploy/docker-compose.observability.yml` (bp 17.3; FR-136)
- [X] T214 [P] [US14] Scrape, ingestion and alert config in `deploy/observability/prometheus.yml`, `deploy/observability/loki-config.yml`, `deploy/observability/promtail-config.yml`, `deploy/observability/alertmanager.yml`, `deploy/observability/prometheus-rules.yml` (bp 17.4, 17.11; FR-136)
- [X] T215 [P] [US14] Zero-click Grafana provisioning in `deploy/observability/grafana/provisioning/datasources/datasources.yml`, `deploy/observability/grafana/provisioning/dashboards/dashboards.yml` (bp 17.5; FR-136)
- [X] T216 [P] [US14] Dashboards: golden signals, logs, cost and tools, infrastructure in `deploy/observability/grafana/dashboards/nexus-golden-signals.json`, `deploy/observability/grafana/dashboards/nexus-logs.json`, `deploy/observability/grafana/dashboards/nexus-cost-and-tools.json`, `deploy/observability/grafana/dashboards/nexus-infrastructure.json` (bp 17.6, 17.7, 17.10, 17.13; FR-136)
- [X] T217 [P] [US14] Per-tool and per-model usage metrics in `internal/reporting/usage.go` (bp 17.9; FR-137)
- [X] T218 [P] [US14] cAdvisor against containerd, plus the label exporter that recovers container names, in `deploy/observability/docker-label-exporter/main.go`, `deploy/observability/docker-label-exporter/Dockerfile` (bp 17.12, 17.15; FR-136)
- [X] T219 [P] [US14] `observability-up` / `observability-down` / `docker-down`, and the doc, in `Makefile`, `docs/observability.md` (bp 17.8; FR-133)
- [X] T220 [P] [US14] Native Langfuse exporter and multi-destination span fan-out, with Tempo as the generic OTLP sink, in `internal/obs/langfuse/exporter.go`, `internal/obs/multi.go`, `internal/obs/tracer.go`, `deploy/observability/tempo.yaml` (post-ledger, `1c4f02d`; FR-138)
- [X] T221 [P] [US14] Opt-in pprof and Pyroscope continuous profiling for `nexusd` and `signerd` in `internal/obs/profiling/pprof.go`, `internal/obs/profiling/pyroscope.go`, `deploy/observability/grafana/dashboards/nexus-profiling.json` (post-ledger, `dda32cf`; FR-139)
- [X] T255 [P] [US14] Promtail Docker service discovery, filtered to the `nexus-agent-demo` compose project, so containerized `nexusd`/`signerd` logs keep shipping, in `deploy/observability/promtail-config.yml` (bp 17.14) — added 2026-11-02: the baseline missed this ledger row

**Checkpoint**: operability, strictly additive.

---

## Phase 17: Polish & cross-cutting concerns

- [X] T222 [P] Architecture narrative in `README.md` and `docs/production-readiness-review.md`; the phase ledger was folded into these specs, with its ids preserved as `(bp N.M)`
- [X] T223 [P] Local-model docs (Ollama + LiteLLM + Langfuse) and the LiteLLM provider adapter in `docs/local-llm.md`, `internal/provider/litellm/litellm.go`, `deploy/docker-compose.local-llm.yml`, `deploy/litellm/config.yaml` (post-ledger; FR-022)
- [X] T224 [P] Contributor guidance, path-scoped rules and Claude Code hooks in `CLAUDE.md`, `.claude/rules/evals.md`, `.claude/rules/migrations.md`, `.claude/hooks/post-edit-go.sh`, `.claude/hooks/pr-size-check.sh`, `.claude/hooks/post-pr-create.sh` (post-ledger)
- [X] T225 [P] Spec Kit scaffolding in `.specify/memory/constitution.md`, `.specify/templates/`, `.claude/skills/speckit-specify/SKILL.md` (post-ledger; `ae5abcb`)
- [X] T226 [P] Spec Kit artifacts for this baseline in `specs/001-agent-platform/spec.md`, `specs/001-agent-platform/plan.md`, `specs/001-agent-platform/research.md`, `specs/001-agent-platform/data-model.md`, `specs/001-agent-platform/quickstart.md`, `specs/001-agent-platform/contracts/kernel-abi.md`
- [ ] T227 Run `specs/001-agent-platform/quickstart.md` end to end against a clean checkout and record the result in `specs/001-agent-platform/checklists/requirements.md`

---

## Phase 18: Post-ledger increments not attached to one story

These shipped after the build ledger was last updated. They are tasked here for completeness; each is already cross-referenced under its story above.

- [X] T228 [P] [US1] Live typing-style preview over SSE, decoupled from the durable log (`OnChunk` → `event: delta`), in `kernel/turns.go`, `internal/runs/rest/broker.go`, `web/src/features/chat/useRunEvents.ts` (post-ledger, `62f052b`; FR-028)
- [X] T229 [P] [US5] Redis Pub/Sub event bus so an SSE client can be served by a different process than the worker in `cmd/nexusd/eventbus.go`, `internal/runs/rest/broker.go` (post-ledger, `2efa4ff`; FR-064)
- [X] T230 [P] [US11] Session history endpoint and sidebar, and a permissive CORS layer for the bearer-token API, in `internal/runs/rest/sessions_list.go`, `internal/surfaces/rest/server.go`, `web/src/features/chat/Sidebar.tsx` (post-ledger, `227b8f4`; FR-123)
- [X] T231 [P] [US11] Timeline folding of the flat event log into chat items in `web/src/features/chat/timeline.ts`, `web/src/features/chat/useRunEvents.ts` (post-ledger, `227b8f4`; FR-123)
- [X] T256 [US5] A run takes the configuration its session pinned — `runstate.ConfigFor` sets the autonomy level, routed model and conversational mode from the session row, and oversight, delegate, plan, runctl and teams all use it instead of each writing the `kernel.RunConfig` literal, which had oversight dropping the model and conversational and delegate and plan dropping conversational, so a conversational chat resumed after an approval or a delegation ended instead of pausing for the next message, and had delegate and teams writing each pinned value twice when they spawn a child. In `internal/runstate/config.go`, `internal/oversight/resume.go`, `internal/delegate/resolve.go`, `internal/delegate/spawn.go`, `internal/plan/steps.go`, `internal/runctl/resume.go`, `internal/teams/lifecycle.go`; `oversight.Sessions.Load` returns the session itself rather than a one-field projection of it. Pinned by `tests/integration/resume_config_test.go` (fails on the old code), `internal/runstate/config_test.go` (every `RunConfig` field must be classified) and `tests/contract/runconfig_test.go` (no feature that runs a session sets a pinned field by hand, no exemption) (post-ledger; FR-029)

---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (1)** → **Foundational (2)** → every story. Foundational blocks all of them.
- **US1 (3)** has no story dependency. **US2 (4)** and **US3 (5)** build on US1's loop but are independently testable (each can run against a stub of the other: `NotImplementedToolExecutor`, `NoopBudgetGate`).
- **US4 (6)** needs US2's pipeline (approval binds to its digest). **US5 (7)** needs US4's checkpoint/approval suspend. **US6 (8)**, **US7 (9)** need US2–US4.
- **US7 (9)** delegation needs the durable suspend from US4; **US10 (12)** reuses US7's taint-fold and envelope patterns.
- **US8 (10)** needs US1's fake provider and the Foundational eval skeleton; plan lifecycle (US7) calls it through an adapter.
- **US9 (11)** hardens what exists; its tasks are independent of each other (`[P]`) except T158 → T159/T161 (typed blocks before thinking/refusal).
- **US11–US14 (13–16)** are additive and ship on a trigger, not a schedule.

### Within each story

Tests that *can* precede implementation are listed first; models/migrations → services → endpoints → integration; story complete before the next priority.

### Parallel opportunities

All `[P]` tasks within a phase touch different files. Phase 11's three ledger sub-phases (13, 14, 15) are mutually independent.

```bash
# US2 — launch together once the pipeline skeleton (T047) exists:
Task: "Tool profiles in internal/permissions/profile.go"            # T051
Task: "Hybrid safety classifier in internal/permissions/safety/"    # T052
Task: "Rule of Two in internal/permissions/ruleoftwo.go"            # T053
Task: "Result budgeting in internal/tools/budget.go"                # T056
```

---

## Implementation Strategy

### Build order actually used (the ledger's)

1. Setup + Foundational (days 1–7) → **US1** (day 12: a working agent over REST) → **US2** (day 20: a *governed* agent — the natural first public milestone) → US3 → US4 → US5 → US6 → US7 → US10-the-eval-gate (US8) → additive stories on a trigger.
2. The eval gate's *skeleton* (T015) precedes the first behavior-bearing slice by constitutional rule; its hardening (US8) comes after US7.
3. Phases 13–15 (US9) ran once, after Phase 12 shipped, before the binary was exposed to anything but a laptop.

### If rebuilding from this spec

MVP = Setup → Foundational → US1 → **stop and validate** (quickstart §2) → US2 → US3 → validate §3–§4 → deploy/demo; then P2 stories in order; P3 stories only when their trigger (README §5) has fired.

### Closing the gaps found while writing this spec

Do the **Convergence backlog** (appended by `/speckit-converge`) in this order: the constitutional **VIOLATION** first (it changes what is allowed), then security **GAPs**, then test-coverage gaps, then documentation drift.

---

## Verification

Task paths were first checked mechanically at `8e02491` (458 backticked repository paths across `T001`–`T231`) and are now checked on every run of the
unit tests by `tests/contract/specs_test.go`: **every path named by a `[X]` task must exist at `HEAD`**. Route names such as `/healthz`, tool ids such as
`platform/delegate`, and the untracked runtime directory `.dev/` are not paths and are skipped. The first run of that test found 21 paths that had moved
since (Phase 19 split `internal/store`, `internal/crypto`, `internal/queue` and others into `postgres/` and `redis/` sub-packages), and they were
corrected in place. `T136` is the one task naming a file that does not exist (`internal/sandbox/broker.go`); it stays unchecked, as a deferred stretch.

Existence is a floor, not a proof. A claim inside a task was additionally spot-checked where the ledger and the code were most likely to differ,
which caught and corrected `T167` (the 1 MiB SSE scanner buffer is in the Go provider adapters, not `web/`). The remaining `[X]` tasks' *behavior*
is what [quickstart.md](quickstart.md) exists to validate.

## Notes

- `[P]` = different files, no dependency on an incomplete task.
- Each story is independently completable and testable; the Phase 3 → Phase 4 → Phase 5 order is the *dependency* order, not a coupling.
- Commit after each task or logical group — and, per `CLAUDE.md`, keep a PR under 1,000 changed lines: **this `specs/001-agent-platform/` directory is itself over that limit** and must be stacked (see the final report).

## Convergence backlog

Appended by `/speckit-converge` against `8e02491`. (This section was headed "Phase 19", which collided with the build ledger's Phase 19, now [spec 002](../002-post-baseline-increments/spec.md).) Ordered constitution findings first (CRITICAL), then test-coverage gaps (HIGH), then MEDIUM and LOW.
Each task names its origin and gap type. Evidence for every finding is in [plan.md](plan.md) (Constitution Check), [spec.md](spec.md) (*Known deviations*) and
[quickstart.md](quickstart.md) (the **No check** rows).

### Re-triage at `09d54f8` (2026-11-02)

The audit ran against `8e02491`, about seven weeks and 40 pull requests ago. This is what checking each task against `HEAD` found. T232–T243 are conformance gaps against the constitution; they moved to specs 003 and 004, where they are specified, and are no longer listed below.

| Tasks | State | Evidence |
|---|---|---|
| T232–T238 | open, moved | [spec 003](../003-trust-surface-conformance/spec.md): the telemetry content channel (`NEXUS_TRACE_CONTENT` still bypasses the allowlist), webhook replay window and per-sender limits (the handlers key by `tenantID`), audit anchoring and alerting (anchors are rows in the same database; a break is only logged), harness-digest pinning (the policy versions are never set) and egress masking (none found) |
| T239–T243 | open, moved | [spec 004](../004-delivery-and-quality-gates/spec.md): no SBOM or signing step in CI, no `docs/durability.md`, no SLO or burn-rate rule, no quality-per-dollar metric in `evals/`, no capability floor in `internal/provider/router.go` |
| T244, T245, T249, T250, T251 | open | no Fork end-to-end test, no tamper-detection test, no SC-024 or SC-021 named tests, no `scripts/check-stack-isolation.sh`, and `Rebaseline` has no caller outside tests |
| T246, T247, T248 | partly covered | `tests/integration/cost_ceiling_test.go`, `internal/cost/pricebook_test.go` and `TestKernel_AwaitingApprovalSuspendsRun` exist; the specific assertions in the tasks were not found |
| T252 | done | the port, the stale README paths, the terminal-reason count, `.dev/skills`, the "simplified JCS" wording and the `sse.ts` comment are all fixed |
| T253 | done | the alias table is in [../README.md](../README.md#legacy-task-number-aliases) |

- [ ] T244 Test `Control.Fork` end to end — a fork at a past sequence with an override runs with external effects disabled, inherits no approvals, has its own budget and audit chain, and reports digest divergence — and the REST and CLI fork paths, in `tests/integration/reliability_test.go`, `internal/runctl/fork.go`, `internal/runctl/rest/handler.go` per SC-017 (US5) (missing)
- [ ] T245 Test tamper detection — alter, delete and reorder a receipt and drop one to open a gap, through the owner connection, and assert `Verify` reports each as a break or gap — in a new `tests/integration/audit_tamper_test.go`, `internal/audit/verify.go` per SC-011 (US4) (missing)
- [ ] T246 Test that a run under a per-task ceiling terminates `cost_exhausted` *before* the overspending provider call and that the recorded `budget_decision` names the refusing budget in `tests/integration/cost_ceiling_test.go` per SC-007 (US3) (missing)
- [ ] T247 Test that a past call's cost is recomputed to the exact minor unit from the price version in force at the time, across a price change, in `internal/cost/pricebook_test.go`, `internal/cost/record.go` per SC-008 (US3) (missing)
- [ ] T248 Test that a run suspended for approval or input makes zero provider calls while suspended and that `suspended_ms` is excluded from active time in `tests/integration/oversight_test.go` per SC-013 (US4) (missing)
- [ ] T249 Pin SC-024 and SC-021 with named tests — the same plan and input take the same branch twice with the transition log naming the predicate that fired; a child cannot return a result that clears the parent's untrusted leg — in `tests/integration/orchestration_test.go` per SC-024, SC-021 (US7) (partial)
- [ ] T250 Automate optional-stack non-interference — start and stop each opt-in compose file and assert the core services stay up and `platform/shell` degrades with a warning — in a new `scripts/check-stack-isolation.sh`, `Makefile` per SC-033 (US13, US14) (partial)
- [ ] T251 Give an operator a way to invoke `TaintState.Rebaseline` (a run-control action and CLI command that clears only the untrusted leg, audited), since nothing outside tests calls it today, in `internal/permissions/ruleoftwo.go`, `internal/runctl/autonomy.go`, `internal/surfaces/cli/cli.go` per FR-039 (US2) (partial)
- [X] T252 Fix documentation drift: the default port (`:8085`), the stale README paths, the terminal-reason count (10, not 8), the `.dev/skills/` path, the "simplified JCS" wording and the stale `X-Nexus-*` header comment, in `CLAUDE.md`, `README.md`, `.env.example`, `internal/tools/execute.go`, `web/src/features/chat/sse.ts`, `specs/001-agent-platform/plan.md` (done)
- [X] T253 Record the legacy task-number aliases (`13.8`–`13.15` → `14.1`–`15.4`) in `specs/README.md`, so code comments that cite them resolve; `tests/contract/specs_test.go` enforces it, per research.md alias table (done)

### Run integrity (2026-11-11)

A review of the control flow found places where a control was designed in one spot and not enforced where the run actually goes: a cost ceiling priced against a model that was never called, an autonomy tightening that never reached a running worker, an approval that resumed outside the queue, a model prompt a hosted API would reject. These are defects against Principles II, IV and V and against FR-wording the baseline already carries, so they are tasks here and not a new feature. Each names the test that pins it; every behaviour change was mutation-checked.

- [X] T254 Show the model one `tool_result` per `tool_use`: a suspension leaves a placeholder result and the resume appended a second, so the first prompt after an approved call or a returned delegation held two for one call. `Rehydrate` picks one answer per call (a real result beats a placeholder, the first real one wins, an orphan is dropped) and `Resume` and `ResumeDelegation` replace the placeholder in place, in `kernel/rehydrate.go`, `kernel/transcript.go`, `kernel/loop.go`, `kernel/rehydrate_test.go`, `kernel/resume_test.go` per the paired-result invariant (Principle II)
- [X] T255 Make the scripted provider refuse a prompt a hosted API would refuse (`provider.ValidateTranscript`: unique call ids, exactly one answer per call, answers before the next assistant turn), so a malformed transcript fails a deterministic test and not a billed call, in `internal/provider/validate.go`, `internal/provider/validate_test.go`, `internal/provider/fake/fake.go` per Principle IX
- [X] T256 Let a mid-run autonomy tightening reach the next tool call: the kernel asks its log for the session's current level before each batch of tool calls (the optional `kernel.AutonomyReader`, implemented by the Postgres and in-memory logs), and the pipeline tightens its pinned level under the permission chain's lock and never loosens it, in `kernel/autonomy.go`, `kernel/ports.go`, `internal/tools/pipeline.go`, `internal/tools/execute.go`, `internal/eventlog/postgres/log.go`, `internal/eventlog/inmem/log.go`, `internal/store/session.go`, `internal/eventlog/logtest/contract.go`, `internal/tools/pipeline_test.go`, `kernel/loop_test.go` per Principle V
- [X] T257 Price a run under the model its provider calls: `provider.ModelBound` (Anthropic, LiteLLM, a failover chain of one model) and `provider.ServedModel`, applied at admission with the router's choice kept in `route_reason`; `nexusd seed` fills missing price entries, prices a LiteLLM model at zero and warns for an unpriced Anthropic model, in `internal/provider/bound.go`, `internal/provider/failover.go`, `internal/provider/anthropic/anthropic.go`, `internal/provider/litellm/litellm.go`, `internal/runs/launch.go`, `cmd/nexusd/compose.go`, `cmd/nexusd/cli_seed.go`, `cmd/nexusd/cli_seed_prices.go`, `.env.example`, `docs/local-llm.md` per Principle IV (the per-model rates of F8 priced models that were never called)
- [X] T258 Keep a run's ceiling across processes and bound the gate's memory: a session's spend is summed from the ledger the first time a process sees it, the price book and tenant budget refresh after `GateConfig.CacheTTL` (serving the last good value if the refresh fails), and per-session ceiling state is evicted after an hour idle, in `internal/cost/gate.go`, `internal/cost/gate_helpers.go`, `internal/cost/gate_cache.go`, `internal/cost/inmem/ledger.go`, `internal/cost/gate_durable_test.go`, `cmd/nexusd/background.go` per Principle IV
- [X] T259 Resume an approved run on a worker: a decision commits, queues a `KindApproval` job carrying only the approval id and answers `202`; the worker calls `Resumer.ResumeDecided` under the session lock, publishing to the bus; a repeated job is a no-op; recovery resumes a run left suspended behind a decided approval; `Job.Payload` survives the stream; the endpoints answer 404 and 409 where they answered 200 with the error in the body (breaking: no `events_appended`), in `internal/oversight/decide.go`, `internal/oversight/resume.go`, `internal/oversight/postgres/unresumed.go`, `internal/oversight/rest/handler.go`, `cmd/nexusd/approvals.go`, `internal/queue/types.go`, `internal/queue/redis/streams.go`, `internal/runctl/recovery.go`, `tests/integration/oversight_resume_test.go`, `bruno/approvals/grant-approval.bru`, `bruno/approvals/deny-approval.bru`, `web/src/features/approvals/ApprovalDetail.tsx`, `specs/001-agent-platform/contracts/rest-api.md` per Principle I (one execution path for a run)
- [X] T260 Write an event and the status it justifies in one transaction (`EventLog.AppendAndSetStatus`), for the terminal event, the approval and delegation suspensions and the conversational pause, so a finished run is never left marked running for orphan recovery to drive again, in `kernel/ports.go`, `kernel/events.go`, `kernel/terminal.go`, `internal/eventlog/postgres/log.go`, `internal/eventlog/inmem/log.go`, `internal/eventlog/logtest/contract.go`, `tests/integration/eventlog_postgres_test.go` per Principle II
- [ ] T261 Join a suspension's approval row and a delegation's bind into the suspension's transaction (the hooks take the event log's transaction), so a failure after `approval_requested` cannot leave a suspended run with no approval to resolve, in `internal/eventlog/postgres/log.go`, `internal/oversight/postgres/store.go`, `internal/delegate/postgres/store.go`, `cmd/nexusd/ports.go` (open: a signature change at about ten integration call sites; see `docs/architecture.md` observation 11)
- [ ] T262 Reconcile a reservation orphaned by a crash between `Reserve` and `Reconcile`: record the reservation id on `budget_decisions` (a migration) and have a periodic pass record an unreported cost for any reservation with no cost record after a grace period, so the ledger agrees with the Redis counter, in `internal/cost/reserve.go`, `internal/cost/reconcile.go`, `internal/cost/postgres/ledger.go`, `migrations/` (open: see `docs/architecture.md` observation 12)
- [ ] T263 Evict the pipeline's per-session state: make delegation read a finished child's Rule-of-Two legs from the child's log instead of `Pipeline.TaintStateFor`, then drop the state when a session ends, in `internal/delegate/resolve.go`, `internal/tools/pipeline.go` (open: evicting first would clear a child's legs from its parent; see `docs/architecture.md` observation 13)
- [ ] T264 Run delegated children and team members as queue jobs instead of detached goroutines, so they get leases, retries, backpressure and graceful shutdown; needs a per-session budget gate so an envelope-budgeted child keeps its envelope on a shared worker, in `internal/delegate/spawn.go`, `internal/teams/lifecycle.go`, `cmd/nexusd/background.go` (open)
- [ ] T265 Add a two-process harness (two `nexusd` workers over one Postgres and Redis) with fault injection between each commit of a transition, so multi-process safety is shown by a test and not by construction, in `tests/integration/` (open; the single-process crash and repeat cases of T259 and T260 are covered)

### Model path, security and operations review (2026-11-13)

A review at `7b94aaa` read the path from a request to the model and back, the tool sandbox and egress, and the queue. It found defects that only show against a hosted model or a run longer than a test: tool names the provider API rejects, memory and per-run tools that never reach the model, no retry of a throttled call, a queue lease shorter than a real run, and a shell that runs unsandboxed when the sandbox cannot start. None was reproduced; each task's first step is a failing test. The detail for every task (where, what to change, the tests, what is already tracked elsewhere and what needs a decision first) is in `docs/review-findings-2026-11.md` under the id in brackets. One task is one pull request.

- [X] T266 Send tool names the provider API accepts: translate `namespace/name@version` to a wire-safe name and back at the adapter boundary, in `internal/provider/wirename.go`, `internal/provider/anthropic/request.go`, `internal/provider/anthropic/stream.go`, `internal/provider/litellm/litellm.go` per Principle VII
- [X] T267 Pin the tenant's memory text and a run's extra tool schemas in the `memory_loaded` and `tool_loaded` events and apply them wherever a run is resumed, so the worker's prompt is the one the session was started with, in `kernel/events.go`, `kernel/types.go`, `kernel/loop.go`, `kernel/turns.go`, `kernel/pinned.go`, `internal/runstate/runstate.go`, `internal/runstate/config.go`, `internal/oversight/resume.go`, `cmd/nexusd/runner.go`, `internal/runstate/config_test.go` per Principles II and III
- [X] T268 Make the provider's output limit a setting (`NEXUS_ANTHROPIC_MAX_TOKENS`) and size the cost reservation's output half from it, in `cmd/nexusd/provider.go`, `cmd/nexusd/cost.go`, `kernel/turns.go`, `.env.example`, `cmd/nexusd/provider_test.go`, `internal/provider/anthropic/anthropic_test.go` per Principle IV
- [X] T269 Retry a provider call classified retryable, before its first chunk, with jittered backoff, a logged reason and the provider's `retry-after`, in `internal/provider/retry.go`, `internal/provider/provider.go`, `internal/provider/anthropic/stream.go`, `internal/provider/litellm/litellm.go`, `cmd/nexusd/compose.go`, `cmd/nexusd/provider.go` per Principle VIII
- [X] T270 Serialise each content block with exactly its own fields, so a thinking block with empty text keeps its `thinking` field and a `tool_use` always has an `input`, in `internal/provider/anthropic/request.go` per Principle VII
- [X] T271 Keep `tool_result` blocks and their call ids when pruning an old tool message, in `internal/promptctx/prune.go` per Principle III
- [X] T272 Condense without splitting a call from its result, show the summariser the real messages, and replay a condensation on rehydrate so a resumed run keeps it, in `internal/promptctx/condense.go`, `internal/promptctx/prune.go`, `kernel/turns.go`, `kernel/transcript.go`, `kernel/events.go`, `kernel/rehydrate.go` per Principle III
- [X] T273 Cache the conversation history as well as the system prompt, in `internal/provider/anthropic/request.go` per Principle III
- [ ] T274 Refuse to start outside `--dev` without a working sandbox, and give the unsandboxed shell an explicit environment instead of the server's, in `cmd/nexusd/pipeline.go`, `internal/tools/builtin/shell.go`, `internal/hooks/exechook/handler.go`, `.env.example` per Principle V (open: R9)
- [ ] T275 Check every redirect against the egress allowlist and refuse a private address at connect time, in `internal/tools/builtin/egress.go`, `internal/tools/builtin/web_fetch.go`, `internal/tools/builtin/connector_fetch.go`, `internal/hooks/httphook/handler.go` per Principle V (open: R10)
- [ ] T276 Put `/metrics` behind a token and cache its response, and require `NEXUS_ADMIN_DATABASE_URL` outside `--dev`, in `cmd/nexusd/config.go`, `cmd/nexusd/http.go`, `cmd/nexusd/surfaces_wiring.go`, `api/openapi.yaml`, `docs/observability.md` per Principle VI (open: R11; relates to spec 004 T033)
- [ ] T277 Verify bearer tokens with a public key file so the server does not hold the signing key, in `internal/authn/authn.go`, `cmd/nexusd/config.go`, `cmd/nexusd/infra.go`, `cmd/nexusd/cli_token.go` per Principle VI (open: R12)
- [ ] T278 Accept a Redis URL with a password and TLS, in `cmd/nexusd/infra.go`, `cmd/nexusd/config.go`, `cmd/nexusd/cli_ingest.go`, `.env.example` per Principle VI (open: R13)
- [ ] T279 Extend a job's lease while it runs, complete a job whose session no longer needs driving without publishing an error, share one circuit-breaker registry and let a breaker close after a cool-down, in `internal/queue/types.go`, `internal/queue/worker.go`, `internal/queue/redis/streams.go`, `internal/reliability/classifier.go`, `cmd/nexusd/background.go` per Principle VIII (open: R14)
- [ ] T280 Sweep for orphaned sessions periodically, skipping one with an unresolved claim and re-enqueueing one at most once in 15 minutes, in `internal/runctl/recovery.go`, `internal/runctl/postgres/orphans.go`, `internal/queue/redis/lock.go`, `cmd/nexusd/background.go` per Principle VIII (open: R15; after T279)
- [ ] T281 Stop leasing on shutdown and let in-flight runs finish within a grace period, and turn a panic in a job into a permanent failure of that job, in `internal/queue/worker.go`, `cmd/nexusd/background.go`, `docs/go-live.md` per Principle VIII (open: R16)
- [ ] T282 Size a cost reservation's input half from the prompt being sent, in `internal/cost/gate.go`, `internal/cost/gate_helpers.go`, `internal/promptctx/builder.go`, `kernel/turns.go`, `kernel/metering.go` per Principle IV (open: R17; after T268)
- [ ] T283 Validate every request the real kernel and resident catalog send against the provider's documented rules in an integration test, and add live eval cases that use the resident catalog, in `internal/provider/anthropic/`, `cmd/nexusd/`, `evals/live_corpus.go` per Principle IX (open: R18; after T266 and T270)
