# Feature Specification: Modular-monolith boundaries

**Feature Branch**: `005-modular-monolith-boundaries`

**Created**: 2026-11-06

**Status**: Implemented, with follow-ups open (see [tasks.md](tasks.md#open-follow-ups)).

**Input**: An architecture review of the codebase against three claims: that it is a **modular monolith**, **packaged by feature**, with **hexagonal** ports and adapters. [Spec 002](../002-post-baseline-increments/spec.md) (Phase 19) made the *persistence* seam real. The review found it true there and not yet true elsewhere: the core still linked an HTTP client, a child-process runner, the OpenTelemetry SDK and a profiler; the composition root held use cases and a Postgres adapter; one package (`internal/store`) held the SQL of five unrelated features; and nothing stopped one feature's adapter reaching into another's.

This is a refactor with no change in behaviour. What it adds is the set of rules that stop the structure drifting back, each shown to fail on a violation.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The core reaches no outside world (Priority: P1)

**Current state (before)**: `go list -deps ./kernel` included `net/http`, `os/exec`, the OpenTelemetry SDK and OTLP exporter, and the Pyroscope client. The cause was four packages that mixed a port with its adapter: `kernel` held the adapter for its own `ToolExecutor` port (and so imported the tool pipeline and, through it, the hook runners), `obs` held the OTLP, Langfuse and profiling clients beside the `Tracer` vocabulary, `hooks` held the command and http handlers beside the dispatcher, and `obs` also held two business features (content-access grants, reports). The boundary tests only guarded the database and the cache.

**Why this priority**: it is the claim "the kernel is the core of the hexagon" and it was only half true. A core that links an HTTP client cannot be unit-tested or reasoned about as pure, and a "physical split later" begins by untangling it.

**Independent Test**: `go list -deps ./kernel | grep -E 'net/http|os/exec|otel/sdk|pyroscope'` prints nothing, and `go test ./tests/contract/ -run TestImportBoundaries` fails if any of the 28 listed packages regains one of those imports.

**Acceptance Scenarios**:

1. **Given** the kernel, **When** its dependency closure is listed, **Then** it contains no HTTP, process, telemetry-SDK, profiler or container-runtime package, and does not contain `internal/tools`.
2. **Given** a core package that gains `import _ "net/http"`, **When** the contract tests run, **Then** they fail and name the package.
3. **Given** `cmd/nexusd`, **When** it starts, **Then** command and http hooks, the OTLP and Langfuse exporters and the profilers are all still registered (the adapters moved; the wiring did not change).

### User Story 2 - Every table has an owner, in its feature's adapter (Priority: P1)

**Current state (before)**: `internal/store` held the SQL of sessions and events, tool claims, checkpoints, the delivery outbox and inbound dedup. `claims` and `deliveries` each had a second, duplicate Go type in `store` beside the feature's own. `tenants`, `messaging_channels` and `derived_artifacts` were written by SQL in `package main`, so no package owned them.

**Why this priority**: package-by-feature means a feature's persistence lives with the feature. Otherwise changing the claim ledger means editing a shared package that five other features also import.

**Independent Test**: `internal/store` contains the tenant scoping, migrations and the session/event aggregate, and nothing else; `grep -rn 'FROM claims\|INTO deliveries' internal/store` finds nothing.

**Acceptance Scenarios**:

1. **Given** the claim ledger, **When** its SQL is looked for, **Then** it is in `internal/runctl/postgres`, returning `runctl.Claim`, with no second type.
2. **Given** the delivery outbox and inbound dedup, **When** their SQL is looked for, **Then** it is in `internal/surfaces/postgres`, using `surfaces.DeliveryStatus`.
3. **Given** the tenants table, **When** a pass needs every tenant, **Then** it asks `tenancy.Directory`, not SQL in a binary.

### User Story 3 - The composition root wires and does not decide (Priority: P1)

**Current state (before)**: `cmd/nexusd` held the post-run checkpoint, the idle-conversation sweep, orphan recovery (a use case with its own tests), the messaging-channel adapter, an audit-chain backend and the derived-artifact recorder, with SQL in seven files. The rule "when a session settles, tell the delegation and the team it belongs to" was hand-copied at two call sites, each of which had to remember it.

**Why this priority**: logic in `main` has no package boundary, so it has no owner, no port and (until recently) no test. A path that forgets the fan-out leaves a delegation's parent waiting for a child that already ended, with no error.

**Independent Test**: `go test ./tests/contract/ -run Composition`; and `go test ./internal/runctl/` exercises the checkpoint, the idle sweep, orphan recovery and the settled observers with no database.

**Acceptance Scenarios**:

1. **Given** a session a worker has just driven as far as it goes, **When** any path (queue worker, approval resume) reports it, **Then** one call, `Control.NotifySettled`, tells every registered observer, and one failing observer does not stop the others.
2. **Given** the serving files of `cmd/nexusd`, **When** they are scanned, **Then** none contains a SQL statement; the operator commands (`cli_*.go`) are the only exemption.
3. **Given** an unreachable observer or a failing checkpoint write, **When** the run has already finished, **Then** the failure is logged and the run's result is unchanged.

### User Story 4 - Features meet through ports, and the rule is enforced (Priority: P2)

**Current state (before)**: `plan/postgres` imported `delegate/postgres` and `runs/postgres` imported `cost/postgres`, because each needed another feature to write a row in the same transaction. `plan` and `runctl` held `*oversight.Approvals`, `*oversight.Inputs` and `*delegate.Delegations` directly. Nothing recorded which feature may depend on which.

**Why this priority**: this is what makes it a *modular* monolith rather than a monolith with packages. It is P2 because nothing was broken; it is what lets the next feature be added without reading the last ten.

**Independent Test**: `go test ./tests/contract/ -run 'TestAdapterIsolation|TestFeatureDependencies'` passes, and fails when `cost` gains `import _ ".../internal/oversight"` or `plan/postgres` gains `import _ ".../internal/delegate/postgres"`.

**Acceptance Scenarios**:

1. **Given** a feature's adapter, **When** it imports another feature's adapter, **Then** the contract tests fail; the shared persistence kit (`store`, `audit/postgres`, `crypto/postgres`, `eventlog/sealed`) is the only adapter code every feature may use.
2. **Given** a new import between two features, **When** the contract tests run, **Then** they fail until the edge is added to `allowedFeatureImports` in the same diff.
3. **Given** a plan with an approval gate, **When** it runs against a fake `plan.ApprovalOpener`, **Then** exactly one approval is opened, the session is suspended on it, and a plan with no approvals wired refuses the gate instead of skipping it.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The kernel MUST import only `internal/{cost,eventlog,obs,promptctx,provider,reliability}`; the tool pipeline reaches it through `internal/tools/kerneladapter`.
- **FR-002**: 28 core and feature packages MUST NOT link `net/http`, `os/exec`, the OpenTelemetry SDK or exporters, the Pyroscope client or a container SDK, even transitively. `connectors`, `skills` and `sandbox` are named exceptions with their reasons recorded in `tests/contract/boundaries_test.go`.
- **FR-003**: A feature's SQL MUST live in that feature's `postgres/` adapter; `internal/store` MUST hold only tenant scoping, migrations and the session/event aggregate.
- **FR-004**: The serving files of `cmd/nexusd` MUST contain no SQL statement.
- **FR-005**: An adapter MUST NOT import another feature's adapter; a same-transaction write across two features is a function the composition root injects, and a request that needs it with none wired MUST be refused, not admitted without it.
- **FR-006**: Every import of one non-adapter package by another MUST be listed in `allowedFeatureImports`.
- **FR-007**: Every path that drives a session to a stop MUST report it through `runctl.Control.NotifySettled`, and an observer MUST be a no-op for a session it does not recognise.
- **FR-008**: The shared kernel (the session/event aggregate, `crypto`, `audit`, the provider port, the persistence kit) and the read model (`reporting/postgres`) MUST be declared in the README as deliberate, not left implicit.

### Key Entities

- **Feature package**: a package under `internal/` (or `kernel/`) that is not an adapter. Holds a feature's vocabulary, its use case and the ports it needs.
- **Adapter**: a package whose last path element is `postgres`, `redis`, `inmem`, `sealed`, `otlp`, `langfuse`, `profiling`, `exechook`, `httphook`, `kerneladapter`, a provider name, `wire` or a test kit. Touches the outside world, or stands in for it.
- **Persistence kit**: `internal/store`, `audit/postgres`, `crypto/postgres`, `eventlog/sealed`. The one way every feature's adapter reaches its tenant-scoped, sealed, receipted transaction.
- **Settled observer**: a feature that cares what became of a session it does not run (`runctl.SettledObserver`).

## Success Criteria *(mandatory)*

- **SC-001**: `go list -deps ./kernel` lists none of the packages named in FR-002.
- **SC-002**: `go build ./... && go vet ./... && golangci-lint run --build-tags=integration ./... && go test -race -tags=integration ./...` passes, and no test asserts anything different. What the moves force are import paths, constructor arguments, and the setup of three tests: the pipeline's rewrite-hook test drives a fake `hooks.Handler` instead of shelling out to `/bin/sh`, the Phase 7 outbox test opens its delivery through the ledger instead of a `store` function, and the inbound-dedup test moved with its code.
- **SC-003**: Each of the five new contract rules fails when a violation is injected, and passes when it is removed.

## Assumptions

- Nothing is deployed, so each move is a straight refactor with no data migration and no compatibility shim (see Phase 19 in spec 002).
- `sessions` and `events` are the shared aggregate root: every feature table has a foreign key to `sessions`, so they cannot be dissolved into one feature without changing the schema. This spec declares that, it does not change it.
