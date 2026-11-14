# Feature Specification: Post-baseline increments: webhook idempotency, hexagonal architecture and a provable topology seam

**Feature Branch**: `002-post-baseline-increments` (retroactive: shipped to `main` through pull requests)

**Created**: 2026-11-02

**Status**: Implemented (retroactive), build-ledger Phases 18, 19 and 20

**Input**: ledger Phases 18-20

> **Retroactive.** This spec was reconstructed from Phases 18, 19 and 20 of the build ledger (`docs/build-phases.md`, deleted; frozen at `09d54f8`), which it replaces. The ledger's text is carried over, not paraphrased; each ledger row is a task in [tasks.md](tasks.md) and keeps its id as `(bp N.M)`, which is what `task N.M` citations in the code resolve to. See [../README.md](../README.md).

Three increments shipped after the baseline ([spec 001](../001-agent-platform/spec.md)) and have nothing in common except that. They share a spec the way 001 shares
one across its stories, so a small increment does not need a directory of its own. A later change that is a feature in its own right gets its own spec.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Inbound-delivery dedup, webhook session locking and atomic conversation resume (ledger Phase 18)

Not part of the spec's 67-pattern coverage — nothing here is a new architectural idea, and like Phase 13 it closes a gap between claimed and shipped behavior rather than adding one. Driven by [`docs/webhook-concurrency-review.md`](../../docs/webhook-concurrency-review.md) (2026-09-18): Phase 11's conversational webhook surfaces (Telegram/Zalo/email) each did an unlocked check-then-act against `store.GetSessionByKey`/`ResumeConversation`, and nothing deduped a provider's own redelivered webhook, so a duplicate or concurrent delivery could double-run a paid LLM turn. 18.1–18.3 close that on the webhook surfaces. 18.4 closes the same check-then-act inside `ResumeConversation` itself, which also covers REST's steer endpoint.

**Independent Test**: send the same Telegram `update_id` twice in a row (a provider retry, or `curl` the webhook endpoint twice with an identical body) — the second call logs `"duplicate delivery acknowledged without dispatching"` and starts no second run; `SELECT count(*) FROM inbound_deliveries` shows exactly one row for it. Fire two concurrent requests at the same conversational `session_key` (two webhook deliveries) — exactly one proceeds and the other logs `"session lock contended"`. Fire concurrent `POST /v1/runs/{id}/steer` calls at a session paused in `awaiting_input` — exactly one returns `200`, the rest are refused, and the transcript gains one `user_message`, never two. **Acceptance**: `go test -tags=integration ./internal/surfaces/... ./internal/store/... ./tests/integration/...` passes, including the new concurrent-delivery tests and `TestRunCtl_ResumeConversation_ConcurrentCallersResumeExactlyOnce` (8 callers released together against a pre-warmed connection pool; it fails 6 runs out of 6 without 18.4's compare-and-set).

### User Story 2 - Hexagonal architecture: persistence and use-case seams (ledger Phase 19)

Not part of the spec's 67-pattern coverage and, like Phases 13 and 18, nothing here is a new architectural idea: it closes the gap between "every seam is a Go interface" and a core that actually has no knowledge of its adapters. An architecture review (2026-10-29) found ports-and-adapters applied at the volatile edges (`provider.Provider`, `queue.Port`, `tools.SandboxExec`, `audit.Signer`, `controlplane.Port`) but not at persistence or the use-case layer: `kernel.Kernel` held a concrete `*store.Store` and passed `pgx.Tx` through its hooks; every surface opened tenant transactions itself; the session-plus-DEK admission sequence existed five times (REST, Telegram, Zalo, email, cron) together with five copies of `RunRequest`/`RunEvent`/ `SealFunc`/`RunStarter`; and the kernel loop could only be exercised against Postgres. Nothing was deployed, so each task below is a straight refactor with no data migration.

**Independent Test**: verified by the tests named in [tasks.md](tasks.md); there is no separate demo command.

### User Story 3 - A provable topology seam, and a session lock that holds (ledger Phase 20)

Not part of the spec's 67-pattern coverage and, like Phases 13, 18 and 19, nothing here is a new architectural idea. Reframing the README around deployment topology and comparing it with the spec's [Deployment topologies](https://github.com/truongpx396/nexus-agent-spec#deployment-topologies) found three claims that rested on nothing that checked them: the control↔data-plane contract (#62) was Go types with no wire encoding and only `AdmitRun` ever crossed it; "any worker can take any job" depended on a session lock that was never renewed (`SessionLock.Renew` existed, and nothing outside its own test called it), so serial-per-session lapsed after the lock's 30 seconds even in one process; and the container images were built by nothing in CI, and had stopped building when `go.mod` moved past the golang image's Go. Nothing is deployed, so each task is a straight change with no data migration.

**Independent Test**: verified by the tests named in [tasks.md](tasks.md); there is no separate demo command.

## Requirements *(mandatory)*

Each requirement is the closes column of one ledger row; its task is in [tasks.md](tasks.md). They are named by ledger id so a later spec can cite them.

### Functional Requirements

#### User Story 1 (closes column of each ledger row)

- **R18.1**: Review finding #1: no inbound dedup on the provider's own delivery id (bp 18.1)
- **R18.2**: The reusable primitives 18.3 wires in (bp 18.2)
- **R18.3**: Review findings #1 and #2, for all three conversational webhook surfaces (bp 18.3)
- **R18.4**: Review finding #2, for REST's own direct-call path (bp 18.4)

#### User Story 2 (closes column of each ledger row)

- **R19.1**: `kernel/` cannot become persistence-free while its vocabulary lives in the Postgres package; two boundary rules were vacuous (bp 19.1)
- **R19.2**: The kernel was only testable against Postgres and its own hook types leaked the driver (bp 19.2)
- **R19.3**: Until now the only way to exercise the loop was `tests/integration` (Docker + PgBouncer, minutes); these run in milliseconds (bp 19.3)
- **R19.4**: Five copies of the run vocabulary and the adapters that existed only to reconcile them (bp 19.4)
- **R19.5**: The admission sequence had no home: it existed five times, so it had no tests of its own (bp 19.5)
- **R19.6**: The admission sequence still lived in four surfaces (bp 19.6)
- **R19.7**: REST was the last surface reading Postgres and the key store inline; its error handling hid failures (bp 19.7)
- **R19.8**: Every PR since the toolchain moved to Go 1.26 failed `lint` and `govulncheck` and so never ran its unit or integration jobs (bp 19.8)
- **R19.9**: The integration job had never run, so a hang and two races in it had never been seen (bp 19.9)
- **R19.10**: The kernel's budget vocabulary lived in a package welded to Postgres and Redis (bp 19.10)
- **R19.11**: The Gate's collaborators had no contract, so a fake of them could not be trusted (bp 19.11)
- **R19.12**: Until now the only way to exercise the Gate was `tests/integration` (Docker, PgBouncer, Redis, minutes) (bp 19.12)
- **R19.13**: `obs` held three Postgres-backed features next to the plain telemetry types the kernel imports (bp 19.13)
- **R19.14**: `Grants` was the last Postgres-backed type between the kernel and a transitive "no database" rule, and the access-to-plaintext path was untested (bp 19.14)
- **R19.15**: Three surfaces carried three copies of the same persistence code, and a fourth surface would have carried a fourth (bp 19.15)
- **R19.16**: The outbox was the last thing in the shared surface package that held the database (bp 19.16)
- **R19.17**: Cron was the one surface that still ran SQL, and the one whose transaction I left alone because moving it could have broken its exactly-once tick (bp 19.17)
- **R19.18**: The resolver ran its own SQL and unwrapped its own keys, so it could only be tested against Postgres (bp 19.18)
- **R19.19**: `internal/crypto` carried a driver into everything that sealed an event, so the tool pipeline and the run path reached Postgres only because the key store lived next to the envelope (bp 19.19)
- **R19.20**: The audit package held both the math that makes the chain tamper-evident and the SQL that stores it (bp 19.20)
- **R19.21**: The same security-relevant sequence existed in seven places, so a fix to any one of them would have had to find the other six (bp 19.21)
- **R19.22**: The approval state machine, which gates every tool call that asks, was welded to `pgx.Tx` (bp 19.22)
- **R19.23**: The state machine that decides whether a tool call may run had no test that did not need Docker (bp 19.23)
- **R19.24**: The approval state machine's own expiry was being discarded, and `on_expiry: default` could never take effect (bp 19.24)
- **R19.25**: Four packages still imported `pgx`/`store` directly, the last of the small ones (bp 19.25)
- **R19.26**: A stale layout section and a maintenance claim that was wrong (bp 19.26)
- **R19.27**: Four services each had a private copy of the append and of the rebuild, and none could be tested without Postgres (bp 19.27)
- **R19.28**: A package that owned both the port and the Redis adapter could not be storage-free (bp 19.28)
- **R19.29**: The run-control service, which is what resumes a crashed run, was welded to pgx (bp 19.29)
- **R19.30**: Delegation, the path by which an agent starts another agent, was untested without Docker (bp 19.30)
- **R19.31**: A completed team could be missing the event that says so, for good (bp 19.31)
- **R19.32**: The orchestration plane's governance record could be missing for a plan that was already signed off (bp 19.32)

#### User Story 3 (closes column of each ledger row)

- **R20.1**: #62's "versioned contract" was Go types only, so a hybrid split had no evidence the contract was sufficient (bp 20.1)
- **R20.2**: per-session serial held only for runs shorter than 30 s (bp 20.2)
- **R20.3**: startup recovery assumed one process (bp 20.3)
- **R20.4**: image rot (bp 20.4)

## Known gaps and notes

### Ledger Phase 20

Not done, and stated so the topology claims stay honest: the other holders of the session lock (the webhook
surfaces, `delegate`) hold it briefly and do not renew; the periodic loops (the audit anchor, the team backstop, the
idle-conversation sweep) have not been audited for several processes; nothing runs two `nexusd` processes together, so
"any replica can take any job" is still a design property, not a tested one; there is no role or mode setting, so
topology is composed in `cmd/nexusd`, not configured; and five of the Port's six operations still have no caller outside
the package. The spec's SSO/OIDC, RBAC, per-tenant rate limits, gVisor/Kata, NATS and BYOC packaging stay out of
scope with their triggers unmet (rows 65 and 66, and [README §5](../README.md#5-explicitly-out-of-scope-and-the-trigger-that-would-change-that)).
