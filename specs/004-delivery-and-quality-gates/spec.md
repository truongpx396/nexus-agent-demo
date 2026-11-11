# Feature Specification: Delivery and quality gates

**Feature Branch**: `004-delivery-and-quality-gates`

**Created**: 2026-11-02

**Status**: Draft (proposed). User stories 1 to 5 are not implemented; story 6 (pipeline hardening and the goroutine-leak check) is. Open decisions are marked `[NEEDS CLARIFICATION]`.

**Input**: The constitution's *Delivery, Scale & Technology Constraints*, *Development Workflow & Quality Gates* and two principles (IV, VII) that the code does not yet meet. `/speckit-converge` found them against `8e02491` (backlog tasks T239–T243 in [spec 001](../001-agent-platform/tasks.md)); each "Current state" was re-checked on 2026-11-02. Requirement ids `FR-401` and up belong to this spec.

Where [spec 003](../003-trust-surface-conformance/spec.md) is about what the running system may do, this one is about how it is built, released and operated.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Durability is defined and rehearsed (Priority: P1)

**Current state**: there is no `docs/durability.md`, and `docs/go-live.md` has no backup or restore item. Nothing defines a recovery point or time objective for the event log or the audit chain, and no restore has been rehearsed.

**Why this priority**: the constitution says "Backup, restore, RPO, and RTO MUST be defined for the event log and audit chain, and restore MUST be rehearsed — an availability SLA without a rehearsed restore is a claim, not a commitment."

**Independent Test**: restore a backup into a clean database and run `nexusd verify-chain` and a projection replay against it.

**Acceptance Scenarios**:

1. **Given** a backup of the event log, the audit chain and the keys, **When** it is restored into an empty database, **Then** `nexusd verify-chain` passes and every session's stored projection equals its replayed one.
2. **Given** the go-live checklist, **When** it is run, **Then** it reports whether a restore has been rehearsed within a stated period, as a manual item printed as a reminder, never a silent pass.

[NEEDS CLARIFICATION: the RPO and RTO numbers, and the backup mechanism (logical dumps, point-in-time recovery, or volume snapshots). **Keys are part of the backup**: the KEK file and the audit signer's key must be restorable, or sealed content is unreadable and the chain cannot be verified, so their custody needs deciding with the mechanism.]

### User Story 2 - Service levels have error budgets and burn-rate alerts (Priority: P2)

**Current state**: `deploy/observability/prometheus-rules.yml` has five alert rules over the golden-signal gauges, whose thresholds spec 001 records as uncalibrated. There are no SLOs, no error-budget policy, no burn-rate alerts and no `docs/runbooks/`.

**Why this priority**: the constitution says every stated availability or latency SLA "MUST have a corresponding SLO, an error-budget policy, and burn-rate alerting that names the runbook it pages to". No SLA is stated today, so the obligation is vacuous until one is; the question is whether to state some.

**Independent Test**: with a synthetic burn, a burn-rate alert fires and its annotation links a runbook that exists.

**Acceptance Scenarios**:

1. **Given** a defined SLO, **When** the error budget burns faster than its window allows, **Then** a fast-burn and a slow-burn alert fire and each names a runbook in `docs/runbooks/`.

[NEEDS CLARIFICATION: which service levels to commit to (run success rate, time to first token, approval latency?), the targets and windows, and who is paged. If none is wanted, the alternative is to state no SLA anywhere, which also satisfies the clause.]

### User Story 3 - The build ships an SBOM and signed artifacts (Priority: P2)

**Current state**: CI runs `govulncheck` on every pull request and weekly, and Dependabot is configured, but no job produces an SBOM or signs anything, and the container images are built in CI but not published.

**Why this priority**: the constitution says "The platform's own build MUST ship an SBOM with signed artifacts and scanned dependencies."

**Independent Test**: build a release; the SBOM lists its dependencies and a signature on each image verifies.

**Acceptance Scenarios**:

1. **Given** a release build, **When** it completes, **Then** an SBOM is attached to it and each published image's signature verifies against a documented identity.

[NEEDS CLARIFICATION: what a "release" is here (a tag, a published image on a registry, neither yet), the SBOM format (SPDX or CycloneDX) and the signing method (keyless signing through the CI provider's OIDC identity is the lowest-maintenance option). Without a publish step there is nothing to sign.]

### User Story 4 - The release gate reports quality-per-dollar and completions-per-million-tokens (Priority: P3)

**Current state**: `evals/efficiency.go` gates token, turn and tool-call bands, but no quality-per-dollar or completions-per-million-tokens figure exists in `evals/`.

**Why this priority**: Principle IV says both "MUST appear in every release gate alongside quality".

**Independent Test**: `make eval` prints both figures for each suite.

**Acceptance Scenarios**:

1. **Given** an eval run, **When** the gate reports, **Then** each suite shows quality per dollar and completions per million tokens next to its pass rate.

[NEEDS CLARIFICATION: gating or reporting only? The default gate runs scripted fake-provider cases whose cost is synthetic, so the figures are only meaningful on the live-model track (the `liveeval` build tag). Report them there and mark the deterministic track "not applicable" rather than invent a price?]

### User Story 5 - Routing has a capability floor (Priority: P3)

**Current state**: `internal/provider/router.go` routes deterministically by data label and difficulty and persists the decision and reason on the session, but has no capability floor for feature demand.

**Why this priority**: Principle VII says routing is "decided by data label and task difficulty (including a capability floor for feature demand), never by model discretion".

**Independent Test**: a table-driven test in which a run that needs a feature the cheapest model lacks is routed to the cheapest model that has it.

**Acceptance Scenarios**:

1. **Given** a run that needs a feature (say, tool calling or a long context), **When** the cheapest eligible model lacks it, **Then** the router picks the cheapest model that has it, and the reason records the floor that applied.

[NEEDS CLARIFICATION: which features count (tool calling, a context-window size, vision, extended reasoning) and where a model's capabilities are declared (the price book, or a model registry beside it).]

### User Story 6 - The pipeline cannot hang, drift or leak goroutines unnoticed (Priority: P2)

**Current state**: before this story, no CI job had a time limit (a testcontainers start that never returned would run to GitHub's 6-hour cap), the `uses:` lines named mutable tags, a `go.mod` that was not tidy was found by whoever next ran `go mod tidy`, tests ran only in source order, a goroutine a test left running failed nothing, and Dependabot opened one PR per dependency. Whether `main` *requires* any of the CI checks is a repository setting, not something a file here can change, and it is not set (T016).

**Why this priority**: a job that can hang for six hours, a check nothing requires and a leak no test notices are the ways a pipeline reports green while something is wrong. The first measurements were cheap: 72 of 72 test packages run clean under `-race -shuffle=on` with goleak once two test-side leaks are closed, in under a minute.

**Independent Test**: `tests/contract/goleak_test.go` fails for a package that starts a goroutine, has tests and has no goleak `TestMain`; `go test -race -shuffle=on ./...` and `go test -race -tags=integration ./...` pass.

**Acceptance Scenarios**:

1. **Given** a package that starts a goroutine in non-test code and has tests, **When** none of its tests' `TestMain` calls goleak, **Then** `go test ./tests/contract` fails and names the package and the one-line fix.
2. **Given** a test that leaves a goroutine running, **When** the package's tests otherwise pass, **Then** the package fails and prints the goroutine's stack.
3. **Given** a CI job that stalls, **When** it reaches its `timeout-minutes`, **Then** it is cancelled; each limit is at least 2.5 times that job's longest healthy run and never under five minutes.
4. **Given** a `go.mod` or `go.sum` that `go mod tidy` would change, **When** CI runs, **Then** the `build` job fails.

### User Story 7 - The Go code follows current idiom, fails closed, and the linter keeps it there (Priority: P3)

**Current state**: a review of the whole tree found no defect `go vet` or the standard linters could see, and a handful they could not: `serve` returned the moment `Shutdown` was called rather than when it finished draining, so the process could exit with requests in flight; `startPeriodic` could be stopped only once and did not wait for its pass; the queue workers were not awaited before the pool they lease through closed; `signerd` held a goroutine for a client that connected and sent nothing; a mistyped `nexusd` subcommand booted a server; `Kernel.Resume` and `ResumeDelegation` turned an unrecognised resolution into an empty, successful `tool_result`; and seventeen response sites (REST handlers, the three webhooks and the `/readyz` and `/metrics` probes, the webhooks and `/readyz` being reachable without a token) put a database or dial error, with its host and SQL, in a response body. Around those, the code predated Go 1.22 to 1.26 idiom (`err == pgx.ErrNoRows`, `interface{}`, hand-written `slices.Contains` and `maps.Copy` loops, `fmt.Errorf` with nothing to format, error *types* named `ErrX`, a local named `max`), none of which the configured linters flagged.

**Why this priority**: none of it was exploitable on a trusted network or visible in a test, which is why it had survived; each item is cheap to fix and the second half is cheap to keep fixed once a linter says so.

A second pass over the same tree found the same class again, deeper: the `runctl` and `contentaccess` handlers told a refusal from a database failure only by status code and answered both with the error's text, `Read` answering 403 for either; the REST kit read request bodies with no limit, so one authenticated request could make the process buffer until it died; ingest inflated an uploaded PDF's streams with no cap (a decompression bomb); and both model providers used `http.DefaultClient`, which has no timeout of any kind, so an endpoint that accepted the connection and never answered held a worker and the session lock it kept renewing for ever.

**Independent Test**: `go test ./kernel -run TestResume` fails if the default case is removed; `go test ./internal/surfaces/rest ./internal/surfaces/telegram ./internal/surfaces/zalo ./internal/surfaces/email ./internal/runs/rest` fails if a response body carries the cause; `golangci-lint run --build-tags=integration ./...` reports the next `err == sentinel`, `interface{}` or shadowed builtin.

**Acceptance Scenarios**:

1. **Given** SIGTERM while a request is in flight, **When** the server shuts down, **Then** `serve` returns only after `Shutdown` has returned, so the request finishes (or the 30 s deadline passes) before the process exits.
2. **Given** a run suspended on an approval, **When** `Resume` receives a decision it does not recognise, **Then** it yields an error and appends no `tool_result`.
3. **Given** a database error behind `GET /v1/runs/{id}`, a webhook's channel lookup or `/readyz`, **When** the request fails, **Then** the response names the operation, not the cause, and the cause is in the log.
4. **Given** a comparison `err == sentinel`, a type assertion on an error, a builtin shadowed by a local, an error type named `ErrX` or a call to package `sort`, **When** CI lints, **Then** it fails.
5. **Given** run control or a content read that the session's state or the caller's grant refuses, **When** the handler answers, **Then** it is 404, 409, 400 or 403 by `errors.Is` on the use case's sentinel, and a storage failure behind the same call is a logged 500 that names only the operation.
6. **Given** a request body past 4 MiB, **When** any route reads it, **Then** the answer is 413 and the process has buffered at most the limit; a PDF stream that inflates past 32 MiB, or 256 MiB across a document, is skipped.
7. **Given** a model endpoint that accepts the connection and sends no headers, **When** a run calls it, **Then** the call fails after the provider's header timeout, while a response that sends headers promptly and streams for longer is delivered whole.

### User Story 8 - A run opens quickly, its queue can be watched, and an erased key is gone (Priority: P3)

**Current state**: a review of the write path and the key custody found four things. A fresh run wrote its opening events, one per resident tool plus its memory and the user's message (17 by default), as 17 separate transactions, each with its own advisory lock, audit receipt and signer round trip, all before `POST /v1/runs` could answer; through Postgres and PgBouncer with a local signer that was a median 35.1 ms. The worker count was a constant of two and nothing on `/metrics` said whether two was enough, because the golden signals come from the database and say nothing about the queue. `Kernel.PrunePolicy` and `Kernel.CondenseThresholdBytes` existed and nothing set them, so a long conversation grew until the provider reported an overflow. And shredding a key flipped its status but kept `wrapped_dek`, so anyone with database access and the KEK file could still unwrap what it sealed: erasure rested on a status check, not on the key being gone.

**Why this priority**: none of it is a correctness failure on a trusted network. The key wipe is the one that matters for the erasure claim; the rest is latency and visibility, cheap to fix and cheap to keep fixed.

**Independent Test**: `go test ./kernel -run TestSeed` fails if the opening events stop being one batch; `go test -tags=integration ./tests/integration -run 'TestShred_|TestMigration_Wipes|TestPostgresEventLog_AppendBatch'` fails if a shredded key keeps its bytes or a failed batch leaves events behind; `go test ./cmd/nexusd -run 'TestWriteQueueGauges|TestQueueWorkers|TestContextPolicy'` fails if the gauges, the worker setting or the context settings change.

**Acceptance Scenarios**:

1. **Given** a new run, **When** it is seeded, **Then** its opening events are one `AppendBatch` (one transaction, a receipt per event, all or nothing), yielded in order and only after they are durable.
2. **Given** a batch whose third receipt cannot be signed, **When** it is appended, **Then** the first two events and their receipts roll back with it, and a later batch is numbered from 1.
3. **Given** a key that is shredded, **When** the transaction commits, **Then** its `wrapped_dek` is empty, its status and `shredded_at` are set, `Unwrap` returns `ErrKeyShredded`, and the event log still replays and the audit chain still verifies; keys shredded before this change are brought to the same state by a migration.
4. **Given** a scrape of `/metrics`, **When** the queue can report its depth, **Then** it carries jobs by state, the age of the oldest job and the worker count; **When** it cannot, **Then** the gauges are left out and the scrape still succeeds.
5. **Given** `NEXUS_QUEUE_WORKERS` outside 1 to 64, **When** `nexusd` starts, **Then** it runs the default and logs why.
6. **Given** neither `NEXUS_CONTEXT_PRUNING` nor `NEXUS_CONDENSE_THRESHOLD_BYTES`, **When** a run goes on, **Then** the model is shown the whole transcript, as before.

## Requirements *(mandatory)*

- **FR-401**: Backup, restore, RPO and RTO MUST be defined for the event log and audit chain, and a restore MUST be rehearsed and recorded (US1).
- **FR-402**: Each stated service level MUST have an SLO, an error-budget policy and burn-rate alerts that name a runbook (US2).
- **FR-403**: A release build MUST produce an SBOM and signed artifacts (US3).
- **FR-404**: Every release gate MUST report quality-per-dollar and completions-per-million-tokens alongside quality (US4).
- **FR-405**: The router MUST apply a capability floor for feature demand, and record it in the routing reason (US5).
- **FR-406**: Every CI job MUST have a timeout sized from measured healthy runs; workflows MUST declare least-privilege permissions and pin each action to a commit; unit tests MUST run shuffled; a `go.mod` or `go.sum` that is not tidy MUST fail the build (US6).
- **FR-407**: A package that starts a goroutine in non-test code MUST run goleak after its tests, enforced by a contract test with no standing exemptions (US6).
- **FR-408**: A server's graceful shutdown MUST finish before the process exits; a periodic pass and the queue workers MUST be stopped and awaited, and a socket server MUST bound a connection's lifetime (US7).
- **FR-409**: A response to a client MUST NOT carry the text of an infrastructure error (a database, queue or dependency failure); it names the operation and the log holds the cause. A resolution the kernel does not recognise MUST stop the run and MUST NOT produce a tool result (US7).
- **FR-410**: Errors MUST be matched with `errors.Is` and `errors.As`/`errors.AsType`, error types MUST be named `XxxError`, and a local MUST NOT shadow a builtin; `.golangci.yml` enforces these and the standard library's newer forms (US7).
- **FR-411**: A use case MUST give an error its caller can act on a sentinel wrapped with `%w`, and a binding MUST map it with `errors.Is`; every other error from it is an infrastructure failure under FR-409. A request body MUST be bounded by the kit and read with `rest.DecodeJSON`, and untrusted input MUST NOT be read, inflated or waited for without a bound (US7).
- **FR-412**: A run's opening events MUST be written as one atomic batch, with a receipt per event, and yielded only once durable (US8).
- **FR-413**: Shredding a key MUST remove its wrapped key bytes in the same statement that marks it shredded; the row, its status and `shredded_at` remain as proof it was retired (US8).
- **FR-414**: The job queue's depth by state, the age of its oldest job and the worker count MUST be exposed on `/metrics`, a queue that cannot report them MUST NOT fail the scrape, and the worker count MUST be configurable and bounded (US8).
- **FR-415**: Pruning and condensation of the model's context MUST be reachable from configuration and MUST be off by default, because enabling either changes what the model is shown and has to clear the eval gate against the model in use (US8).
