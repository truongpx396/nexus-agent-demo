# Implementation Plan: Nexus Agent Platform (baseline)

**Branch**: `001-agent-platform` (retroactive) | **Date**: 2026-10-02 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/001-agent-platform/spec.md`

**Note**: Retroactive. This plan documents the architecture *as built* at `8e02491` and audits it against the
constitution; it is not a proposal. The build order it records is `docs/build-phases.md` Phases 0–17 plus the
post-ledger increments (Phase 18 in [tasks.md](tasks.md)).

## Summary

A single-binary Go reimplementation of `truongpx396/nexus-agent`: one kernel loop (an `iter.Seq2` generator) driven by a
durable queue, reaching the world through thin surfaces, governed by a 16-step tool pipeline and a 10-layer permission
chain, bounded by pre-spend cost reservation, and grounded in an append-only, envelope-encrypted, hash-chain-audited
event log under forced row-level security. The control-plane/data-plane split is a Go interface rather than a network
boundary. The governing design rule, inherited from the original's MVP cut line: **seams and schema are decided early
because they are expensive to retrofit; infrastructure is added late because it is cheap** — so the demo keeps every
seam, contract and control and collapses only deployment topology, scale-out infrastructure and the commercial tail.

## Technical Context

**Language/Version**: Go 1.25.14 (toolchain pinned in `go.mod`); TypeScript ~6.0 for the web app

**Primary Dependencies**: `pgx/v5` (Postgres), `go-redis/v9` (Streams, Lua counters, locks, Pub/Sub), `zerolog`, `uuid`,
`golang-jwt/v5` (dev token issuer), OpenTelemetry + a native Langfuse exporter + Pyroscope (opt-in), the Docker engine API
and the OpenSandbox Go SDK (sandboxing), `testcontainers-go` (tests), `gopkg.in/yaml.v3` (eval corpus); React 19 + Vite 8
+ react-router 7 (web). The Anthropic adapter is hand-rolled over HTTP behind the provider port.

**Storage**: PostgreSQL 17 (direct for migrations only) behind **PgBouncer in transaction-pooling mode** (every runtime
connection) with `pgvector`; Redis 7 with append-only persistence (queue, session-key locks, atomic cost counters, event
bus); a local blob directory for oversized tool output; file-backed KEK and signing key (`.dev/` in dev).

**Testing**: `go test` — unit and property tests need no external service; `-tags=integration` starts disposable
Postgres + PgBouncer (+ Redis) through testcontainers; `-tags=liveeval` for the only tests that call a live model. An
eval gate (`make eval`) is a CI job. Correctness tests use `internal/provider/fake`, never a live model.

**Target Platform**: Linux containers (distroless, non-root) and macOS/Linux dev hosts; Docker for sandboxes.

**Project Type**: web-service + CLI + web app — three Go binaries (`nexusd`, `nexusctl`, `signerd`) and `web/`.

**Performance Goals**: cache-read ≥ 90% on steady-state turns (SC-003); a *measured* submission rate for `POST /v1/runs`
on the fake provider (SC-034, `tests/integration/load_bench_test.go`). No latency SLO is declared — see Constitution Check.

**Constraints**: tenant scope transaction-local only; no float in money; non-test Go files ≤ 500 lines; PRs ≤ 1,000
changed lines; telemetry content-free; every model call metered; `nexusd` cannot append an event unless `signerd` answers.

**Scale/Scope**: 32 tables from 25 migrations (`0000`–`0024`), 60 event types, 10 terminal reasons, 16 pipeline steps,
10 permission layers, 8 surfaces (REST, CLI, MCP, Telegram, Zalo, email, cron, web), 141 Go test files across unit,
property, contract and integration tiers. One developer, 18 ledger phases (0–17) plus post-ledger increments.

## Constitution Check

*GATE: audited against `.specify/memory/constitution.md` v1.2.0 at `8e02491`.* Verdicts: **PASS** (enforced
mechanically or by test), **PASS\*** (met with a documented simplification), **GAP** (a MUST with no shipped evidence),
**VIOLATION** (the shipped code contradicts a MUST).

| # | Principle / constraint | Verdict | Evidence or finding |
|---|---|---|---|
| I | One loop, many surfaces | PASS | `tests/contract/boundaries_test.go` (transitive import graph); `kernelRunStarter` is the one place a `RunState` is built. |
| II | Immutable models, append-only state; paired results | PASS | `events_forbid_mutation` trigger + `REVOKE`; `tests/property/paired_result_test.go`. |
| III | Cache-stable context | PASS | Two-zone builder, prefix byte-equality test, wire-level cache breakpoint (F4 closed). |
| IV | Stop on cost, not vibes | PASS\* | Reserve→reconcile gate, `cost_exhausted`. **GAP (sub-clause):** "quality-per-dollar and completions-per-million-tokens MUST appear in every release gate" — `evals/efficiency.go` gates tokens/turns/tool-calls bands but no quality-per-dollar metric exists. |
| V | Safety per-invocation, fails closed, Rule of Two | PASS | 16-step pipeline, 10-layer chain, hybrid classifier, durable taint projection. |
| VI | Tenant first; audit and observability day-one | PASS | Forced RLS on 32 tables, isolation test through PgBouncer, hash-chained receipts. |
| VII | Provider-agnostic | PASS\* | One port; Anthropic, LiteLLM and fake adapters; deterministic router. **GAP (sub-clause):** routing "including a capability floor for feature demand" — no capability-floor concept found in `internal/provider/router.go`. |
| VIII | Classify, resume, never silently retry | PASS | Classifier, circuit break at 3, checkpoint resume, stuck detection. "Deploys MUST NOT cut a running agent over mid-task" is met by checkpoint resume plus graceful shutdown; rainbow deploy is out of scope. |
| IX | Verify against acceptance; govern change | PASS | Eval gate is Phase 1 deliverable; deterministic provider. Agent-written skills: N/A (none are written by agents). |
| — | Secrets in a vault, never in the prompt | PASS\* | Per-tenant sealed credentials; the "vault" is a file-backed KEK by design (README §2 collapse). |
| — | Identity: act as the calling user | PASS\* | Per-turn principal and bearer claims; tool-boundary RBAC is the tool profile + chain, not a separate RBAC engine. |
| — | Audit receipts hash-chained, anchored *outside the writing system*, sign-only, scheduled verifier | PASS\* | `signerd`, `internal/audit/{chain,anchor,verify}`; `nexusd` cannot import `signerkey` (contract test); receipts and anchors are append-only by trigger + `REVOKE`; a ticker in `cmd/nexusd/background.go` runs `Anchor` then `Verify`. **GAP (sub-clause):** anchors are rows in the **same Postgres** the writer uses — "outside the transactional writer path" (`anchor.go`), not outside the writing system. Nothing is published to a separate system, so (as read from the code) a database superuser who removes the chain tail *together with its anchors* would not be detected. |
| — | **Egress & redaction**: PII/secrets/PHI/card data masked by class before leaving the boundary | **GAP** | Domain allowlist exists (web fetch, connectors). No class-based masking component found; `redact` hits are telemetry/reasoning redaction only. |
| — | Content at rest / erasure by crypto-shredding | PASS | `internal/crypto`; erasure test proves replay and chain verification survive. |
| — | Human-in-the-loop; approval expiry denies the action | PASS | `internal/oversight`; `approval_expired` → typed denial. |
| — | **Inbound channel authenticity**: verify provider authenticity, reject replays in a bounded window, rate-limit *per external identity*, all before the kernel sees the payload | **GAP** (partial) | Authenticity **is** verified before the body is parsed, in constant time: Telegram secret header, Zalo HMAC over the raw body, email Basic Auth. Missing: **no replay window** on any of the three (no timestamp or nonce check), and the limiter is keyed by `tenant_id`, not by external sender — one abusive sender can exhaust a tenant's whole channel. |
| — | **Compliance: SBOM with signed artifacts** | **GAP** | `govulncheck` and Dependabot exist (F11); no SBOM or artifact signing in CI. |
| — | Control plane / data plane split behind a versioned contract | PASS | `controlplane.Port` v1 + `LocalPort`; boundary rules active (F15). |
| — | Config, not forks | PASS | Tenant/agent/profile/policy are rows and markdown. |
| — | Stateless workers, durable queue, session-key routing | PASS\* | Redis Streams consumer group + session lock. Warm sandbox pool is out of scope (README §5). |
| — | Memory is files first | PASS | `internal/memory`; retrieval added only past the trigger (Phase 12). |
| — | Cost ceilings enforced before the spend | PASS | Reservation + worker-local ceiling. |
| — | Event log is a versioned contract; schema change is expand/contract | PASS | `schema_version` + upcast registry; migrations expand-only (`0024` is a cleanup step after the adapter swap). |
| — | **Durability: backup, restore, RPO, RTO defined; restore rehearsed** | **GAP** | Nothing in the repo defines or rehearses them (constitution text only). Go-live item 10 is a manual reminder. |
| — | **SLOs have error budgets and burn-rate alerting naming a runbook** | **GAP** | Five alert rules exist (`deploy/observability/prometheus-rules.yml`), and its header says they are *not* calibrated SLOs; no error-budget policy or runbook link. |
| — | State artifacts not interchangeable; write-ahead claims; replay as three operations; digest persisted | PASS\* | Three artifacts, three runctl operations, `harness_digest` persisted at run start. **GAP (sub-clause):** "every run MUST persist the digest of the configuration that determined its behavior" — `harness.Config.SafetyPolicyVersion` and `ApprovalPolicyVersion` are never set by any non-test caller, so the digest does not move when those policies change. |
| — | **Observability captures structure, not content — "no flag may admit content"** | **VIOLATION** | `NEXUS_TRACE_CONTENT=true` (default off) attaches prompt/completion/tool I/O to spans via `obs.Span.SetContent`, bypassing the allowlist, with no receipt. Docs call it a "sanctioned exception"; the constitution's only exception is the receipt-bearing content-access grant. See Complexity Tracking. |
| — | Go-live gate | PASS\* | `nexusd go-live`: six automated items, four manual reminders. |
| — | Build for the current stage | PASS | Each deferral has a trigger in README §5; the ledger records a cut line and an out-of-scope list. |

**Gate result**: **FAIL on one VIOLATION and nine GAPs** (quality-per-dollar gating, capability-floor routing, class-based
egress masking, webhook replay/per-identity limiting, SBOM and artifact signing, backup/restore RPO/RTO, SLO error budgets,
configuration digest missing policy inputs, audit-chain anchor not external to the datastore).
The plan proceeds because this is a baseline of shipped work, not a
proposal to ship; each finding becomes a task in the Phase 19 convergence block of [tasks.md](tasks.md) rather than being
waved through. **Re-check after Phase 1 design**: unchanged — the data model and contracts add no new finding, and
confirm VIOLATION 1 is the only place a content-bearing channel crosses the telemetry boundary.

## Phase 0 / Phase 1 outputs

| Artifact | Purpose |
|---|---|
| [research.md](research.md) | Decisions with rationale and rejected alternatives, the legacy task-number alias table, and the F1–F15 audit trail |
| [data-model.md](data-model.md) | The 32 tables, relationships, state machines and validation rules |
| [contracts/](contracts/) | Kernel ABI, tool/permission contract, orchestration plane, REST/SSE/webhook API, control-plane `v1`, signer protocol, event taxonomy |
| [quickstart.md](quickstart.md) | Runnable validation scenarios, one per story |

## Story → ledger → package map

| Story | Ledger phase | Primary packages |
|---|---|---|
| Foundations | 0–1 | `migrations/`, `internal/{store,crypto,obs,harness}`, `internal/provider/fake`, `evals/`, `tests/contract` |
| US1 | 2 | `kernel/`, `internal/{promptctx,provider,surfaces/rest}` |
| US2 | 3 | `internal/{tools,tools/builtin,permissions,hooks}` |
| US3 | 4 | `internal/cost` |
| US4 | 5 | `cmd/signerd`, `internal/{audit,oversight,sandbox}`, `internal/crypto/shred.go`, `internal/obs/grant.go` |
| US5 | 6, 18 | `internal/{queue,runctl,reliability}`, `internal/store/{checkpoint,snapshot,claims}.go` |
| US6 | 7 | `internal/{memory,skills,surfaces,surfaces/cli}` |
| US7 | 8 | `internal/{plan,delegate}`, `internal/tools/builtin/delegate.go` |
| US8 | 10 | `evals/`, `internal/evalgate`, `cmd/nexusd/cli_golive.go` |
| US9 | 13–15 | `internal/{authn,config,controlplane}`, `cmd/nexusd/{http,serve,config}.go`, `Dockerfile` |
| US10 | 9 | `internal/teams`, `internal/tools/builtin/board.go` |
| US11 | 11, 18 | `internal/surfaces/{mcp,telegram,zalo,email,cron,web}`, `internal/connectors`, `web/` |
| US12 | 12 | `internal/{ingest,retrieval}`, `internal/tools/builtin/retrieve.go` |
| US13 | 16 | `internal/tools/builtin/web_crawl.go`, `internal/sandbox/opensandbox.go`, `deploy/docker-compose.agentic.yml`, `.dev/skills/` |
| US14 | 17, 18 | `internal/obs/{logging,usage,langfuse,multi,pprof,pyroscope}.go`, `deploy/observability/` |

## Project Structure

### Documentation (this feature)

```text
specs/001-agent-platform/
├── spec.md              # /speckit-specify
├── plan.md              # this file (/speckit-plan)
├── research.md          # Phase 0
├── data-model.md        # Phase 1
├── quickstart.md        # Phase 1
├── contracts/           # Phase 1
│   ├── kernel-abi.md
│   ├── tool-contract.md
│   ├── orchestration-plane.md
│   ├── rest-api.md
│   ├── control-plane-v1.md
│   ├── signer-protocol.md
│   └── event-taxonomy.md
├── checklists/requirements.md
└── tasks.md             # /speckit-tasks (+ /speckit-converge appends)
```

### Source Code (repository root)

```text
cmd/
├── nexusd/              # composition root: control plane + workers + REST; subcommands migrate|seed|serve|verify-chain|dashboard|go-live|erase|ingest|token
├── nexusctl/            # CLI surface (thin wrapper over internal/surfaces/cli)
└── signerd/             # sign-only audit key custody over a unix socket
kernel/                  # THE LOOP — imports neither internal/surfaces nor internal/controlplane
internal/
├── provider/ (anthropic, litellm, fake) · tools/ (+builtin) · permissions/ (+safety) · hooks/ · oversight/
├── promptctx/ · memory/ · skills/ · cost/ · audit/ · crypto/ · store/ · queue/ · reliability/ · runctl/
├── sandbox/ · plan/ · delegate/ · teams/ · harness/ · config/ · authn/ · controlplane/ · evalgate/
├── surfaces/ (rest, cli, mcp, telegram, zalo, email, cron, web, capability) · connectors/
├── retrieval/ · ingest/ · obs/ · dotenv/ · version/
migrations/              # 0000–0024, embedded, expand-only
evals/                   # corpus/*.yaml, runner, graders, judge, digest, baseline.json
tests/{contract,integration,property}/
web/                     # React app over the public REST + SSE API
deploy/                  # docker-compose{,.local-llm,.agentic,.observability}.yml, sandbox image, observability configs
docs/                    # constitution, build-phases, go-live, local-llm, agentic-capabilities, observability, readiness review
```

**Structure Decision**: single Go module with one composition root (`cmd/nexusd`) and mechanically enforced import
boundaries (`tests/contract/boundaries_test.go`): `kernel/` may not import surfaces or controlplane; surfaces may not
import `kernel/`; `controlplane` may not import `sandbox|memory|provider`; the three binaries may not import each other;
`nexusd` may not import `internal/audit/signerkey`. Large files split by responsibility *within the package*, never into a
new package, because the boundary test is package-granular.

## Complexity Tracking

| Violation | Why it exists | Simpler alternative rejected because |
|---|---|---|
| `NEXUS_TRACE_CONTENT` content-bearing span channel (Principle VI / *Observability captures structure, not content*) | Local debugging against a small local model is unusable when every span is content-free (`docs/local-llm.md`). | It cannot simply stay: the constitution says no flag may admit content and that the only path to plaintext carries a receipt. **Decision needed from a maintainer**, one of: (a) remove the flag; (b) amend the constitution (MINOR) to permit a dev-only content channel *and* require it be refused outside `--dev` and emit a receipt; (c) route it through a content-access grant. This plan does not choose. |
| Redis-backed queue replacing the Postgres adapter (Principle: durable queue) | The Postgres adapter's lease expiry was written but never read, so it could not reclaim an abandoned job. | Retaining it would have kept a capability the platform claimed but did not have; see research.md R-10. Not a violation — recorded because it reverses a ledger row (#41). |
