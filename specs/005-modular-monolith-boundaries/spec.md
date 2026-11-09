# Feature Specification: Modular-monolith boundaries

**Feature Branch**: `005-modular-monolith-boundaries`

**Created**: 2026-11-06

**Status**: Implemented. One tooling follow-up stays open on purpose (see [tasks.md](tasks.md#open-follow-ups)).

**Input**: An architecture review of the codebase against three claims: that it is a **modular monolith**, **packaged by feature**, with **hexagonal** ports and adapters. [Spec 002](../002-post-baseline-increments/spec.md) (Phase 19) made the *persistence* seam real. The review found it true there and not yet true elsewhere: the core still linked an HTTP client, a child-process runner, the OpenTelemetry SDK and a profiler; the composition root held use cases and a Postgres adapter; one package (`internal/store`) held the SQL of five unrelated features; and nothing stopped one feature's adapter reaching into another's.

A second pass (User Stories 5 to 7) took the findings the first pass declared and left: the operator commands' SQL, an erasure that wrote another feature's table, REST handlers and model tools grouped by transport and by kind, two packages still linking infrastructure, a web app and a test directory organised by kind of file or by build phase.

This is a refactor with almost no change in behaviour (the two exceptions are named in FR-011 and FR-012: a skill script with no sandbox is refused rather than run, and an event type outside the taxonomy is refused at append). What it adds is the set of rules that stop the structure drifting back, each shown to fail on a violation.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The core reaches no outside world (Priority: P1)

**Current state (before)**: `go list -deps ./kernel` included `net/http`, `os/exec`, the OpenTelemetry SDK and OTLP exporter, and the Pyroscope client. The cause was four packages that mixed a port with its adapter: `kernel` held the adapter for its own `ToolExecutor` port (and so imported the tool pipeline and, through it, the hook runners), `obs` held the OTLP, Langfuse and profiling clients beside the `Tracer` vocabulary, `hooks` held the command and http handlers beside the dispatcher, and `obs` also held two business features (content-access grants, reports). The boundary tests only guarded the database and the cache.

**Why this priority**: it is the claim "the kernel is the core of the hexagon" and it was only half true. A core that links an HTTP client cannot be unit-tested or reasoned about as pure, and a "physical split later" begins by untangling it.

**Independent Test**: `go list -deps ./kernel | grep -E 'net/http|os/exec|otel/sdk|pyroscope'` prints nothing, and `go test ./tests/contract/ -run TestImportBoundaries` fails if any of the 33 listed packages regains one of those imports.

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
2. **Given** the files of `cmd/nexusd`, **When** they are scanned, **Then** none contains a SQL statement, the operator commands included.
3. **Given** an unreachable observer or a failing checkpoint write, **When** the run has already finished, **Then** the failure is logged and the run's result is unchanged.

### User Story 4 - Features meet through ports, and the rule is enforced (Priority: P2)

**Current state (before)**: `plan/postgres` imported `delegate/postgres` and `runs/postgres` imported `cost/postgres`, because each needed another feature to write a row in the same transaction. `plan` and `runctl` held `*oversight.Approvals`, `*oversight.Inputs` and `*delegate.Delegations` directly. Nothing recorded which feature may depend on which.

**Why this priority**: this is what makes it a *modular* monolith rather than a monolith with packages. It is P2 because nothing was broken; it is what lets the next feature be added without reading the last ten.

**Independent Test**: `go test ./tests/contract/ -run 'TestAdapterIsolation|TestFeatureDependencies'` passes, and fails when `cost` gains `import _ ".../internal/oversight"` or `plan/postgres` gains `import _ ".../internal/delegate/postgres"`.

**Acceptance Scenarios**:

1. **Given** a feature's adapter, **When** it imports another feature's adapter, **Then** the contract tests fail; the shared persistence kit (`store`, `audit/postgres`, `crypto/postgres`, `eventlog/sealed`) is the only adapter code every feature may use.
2. **Given** a new import between two features, **When** the contract tests run, **Then** they fail until the edge is added to `allowedFeatureImports` in the same diff.
3. **Given** a plan with an approval gate, **When** it runs against a fake `plan.ApprovalOpener`, **Then** exactly one approval is opened, the session is suspended on it, and a plan with no approvals wired refuses the gate instead of skipping it.

### User Story 5 - Every table has one owner, and the rule is enforced (Priority: P1)

**Current state (before)**: the first pass moved the feature SQL out of `store` and `main` but did not stop it coming back. Erasure (`crypto/postgres`) still deleted and tombstoned rows in `derived_artifacts`, which the tool pipeline owns. The operator commands inserted tenants, counted budgets and looked up a session's tenant with raw SQL in `cmd/nexusd`. `store` still held two `runctl` queries and the `eventlog` snapshot cache. Nothing listed which feature owns which table.

**Why this priority**: package-by-feature is a claim about data as much as code. A table written from two features is a table nobody can change alone, and it is the part that makes extracting a feature hard.

**Independent Test**: `go test ./tests/contract/ -run TestTableOwnership` passes, and fails for an injected query on another feature's table, on a kernel table from an unlisted package, on a write from the read model, and for a migration that adds a table with no owner.

**Acceptance Scenarios**:

1. **Given** an erasure, **When** it removes a session's derived artifacts, **Then** the rows are deleted by the artifact owner's writer, injected into `cryptopg.Eraser`, in the same transaction as the key shred; an Eraser given no writer fails instead of erasing partially.
2. **Given** `erase --session`, `seed` and `go-live`, **When** they need a tenant, a registration or a budget, **Then** they call `tenancy` and the cost adapter, and `cmd/nexusd` holds no SQL.
3. **Given** a new migration with a `CREATE TABLE`, **When** the contract tests run, **Then** they fail until the table is listed with its owner.

### User Story 6 - A feature's driving adapters sit beside it (Priority: P1)

**Current state (before)**: REST handlers for four features shared one transport package, and because that package may not reach the kernel, two translation structs in `cmd/nexusd` (about 150 lines, and they held logic: draining a resume stream and notifying observers; choosing between steering and continuing by reading the session) stood between the handlers and the features. The three tools that expose one feature to the model (board, delegate, retrieve) lived in a catalog of every tool and kept primitive-typed copies of the feature's types, with three more translation structs in `cmd/nexusd`.

**Why this priority**: a transport is not a feature. Grouping handlers by it makes every feature's API a change in a shared package, and puts feature logic in the composition root.

**Independent Test**: `go test ./tests/contract/ -run 'TestFeatureDependencies|TestImportBoundaries'` passes, fails when a binding imports storage, the kernel or an undeclared feature, and `go test ./internal/oversight/... ./internal/runctl/...` exercises the handlers and the use cases they call without a database.

**Acceptance Scenarios**:

1. **Given** the REST surface, **When** a route is registered, **Then** it is registered through `Router.Handle` and so authenticated, and a path whose module is not mounted answers 404 without asking for a token.
2. **Given** an approval decision, **When** the resumed run settles, **Then** `oversight.Decisions` tells the settled observers once, and only if the resume succeeded.
3. **Given** a message to a session, **When** it is paused in `awaiting_input`, **Then** `runctl.Control.Send` continues the conversation, and otherwise steers; the handler reads no session.
4. **Given** the delegate tool, **When** its bounds change in `delegate`, **Then** the tool changes with them, because it uses `delegate.MaxDepth/MaxConcurrent/MaxPerRun`.

### User Story 7 - The remaining leaks, and the layer-shaped packages (Priority: P2)

**Current state (before)**: `connectors` carried `golang.org/x/oauth2` types in its API and `skills` fell back to a local `/bin/sh`, so neither could join the no-infrastructure list. The event log's suspend and delegate hooks took a raw `pgx.Tx` (the approval hook ignored it and opened a second transaction). The event taxonomy named no owner. `web/src` was `components/`, `lib/` and `pages/` with one `api.ts` and one `types.ts`; `tests/integration` named eleven files for the build phase.

**Why this priority**: none of it was broken. It is the difference between a structure that is mostly by feature and one that is.

**Independent Test**: `go list -deps ./internal/connectors ./internal/skills` lists no HTTP client or process package; `cd web && npm run lint && npm run build` passes on the feature layout.

**Acceptance Scenarios**:

1. **Given** the connector vault, **When** a token is refreshed, **Then** the vault speaks only `connectors.Token` and the `Authorizer` port, and the OAuth library is linked by `connectors/oauth2` alone.
2. **Given** a skill script and a run with no sandbox, **When** the tool is called, **Then** it is refused with `skill_script_requires_sandbox` and no process starts.
3. **Given** an event type added to `event.go` with no owner in `taxonomy.go`, **When** the unit tests run, **Then** they fail; and `Append` of a type outside the taxonomy fails in both adapters.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The kernel MUST import only `internal/{cost,eventlog,obs,promptctx,provider,reliability}`; the tool pipeline reaches it through `internal/tools/kerneladapter`.
- **FR-002**: 33 core and feature packages MUST NOT link `net/http`, `os/exec`, `golang.org/x/oauth2`, the OpenTelemetry SDK or exporters, the Pyroscope client or a container SDK, even transitively. `sandbox` is the one named exception, with its reason recorded in `tests/contract/boundaries_test.go`.
- **FR-003**: A feature's SQL MUST live in that feature's `postgres/` adapter; `internal/store` MUST hold only tenant scoping, migrations and the session/event aggregate.
- **FR-004**: No file of `cmd/nexusd` MUST contain a SQL statement; there is no exemption for the operator commands.
- **FR-005**: An adapter MUST NOT import another feature's adapter; a same-transaction write across two features is a function the composition root injects, and a request that needs it with none wired MUST be refused, not admitted without it.
- **FR-006**: Every import of one non-adapter package by another MUST be listed in `allowedFeatureImports`.
- **FR-007**: Every path that drives a session to a stop MUST report it through `runctl.Control.NotifySettled`, and an observer MUST be a no-op for a session it does not recognise.
- **FR-009**: Every table a migration leaves in place MUST be listed with one owning feature, and SQL naming a table MUST be inside its owner's package tree. The `sessions` and `events` tables MUST list each package that may touch them, with the reason; the `reporting` read model MAY read any table and MUST NOT write one.
- **FR-010**: A feature reachable over HTTP or by the model MUST carry that binding in the feature (`<feature>/rest`, `<feature>/tool`). A REST module MUST register routes only through the kit's authenticating `Router.Handle`. A binding MUST NOT import storage or the kernel directly, and MUST declare its imports in `allowedFeatureImports`.
- **FR-011**: `connectors` MUST speak OAuth2 only through its `Authorizer` port, with the protocol library linked by `connectors/oauth2` alone. A skill script MUST run only through the run's sandbox and MUST be refused where there is none.
- **FR-012**: The event log's suspend and delegate hooks MUST NOT take a transaction. Every event type MUST name an owning package, and a log MUST refuse to append a type outside the taxonomy.
- **FR-013**: The web app MUST be packaged by feature, mirroring the backend, and the integration tests MUST be named for the feature they exercise, not the build phase that introduced them.
- **FR-014**: A feature table MUST NOT have a foreign key into `sessions` (only `events` and `sessions` itself may), and `sessions` MUST have exactly the columns in `aggregateColumns` (`tests/integration/session_migration_test.go`). What a feature records about a session is a table of the feature's own, keyed by `session_id`: `team_members` (teams), `session_lineage` (delegate, written for teams through an injected function), `session_forks` (runctl), `plan_sessions` (plan). `sessions` keeps `parent_session_id`, the one generic fact about the spawn tree, and the run configuration the kernel itself consumes (`autonomy_level`, `conversational`).
- **FR-015**: No file name, Go identifier or Go string literal MAY contain "phase" (`tests/contract/naming_test.go`); the digest defaults every run starts under are `harness.DefaultSystemPromptVersion` and `harness.DefaultPromptMode`. The `Phase N` comment citations are exempt: they are the traceability convention `specs_test.go` checks.
- **FR-008**: The shared kernel (the session/event aggregate, `crypto`, `audit`, the provider port, the persistence kit) and the read model (`reporting/postgres`) MUST be declared in the README as deliberate, not left implicit.

### User Story 8 - The session aggregate carries only its own columns, and nothing references it (Priority: P1)

**Current state (before)**: `sessions` was the one table every feature touched. Every feature table had a foreign key to it (about twenty), and four features stamped their own facts on its rows: `team_id` for teams (with a foreign key from the aggregate to a feature table), `root_session_id`, `depth` and `delegation_role` for delegate and teams, the fork columns for `runctl.Fork`, and `plan_id`/`plan_version` for plan. `parent_session_id` existed but nothing wrote it. Extracting a feature would have meant changing the schema of every session first. Separately, names still carried build phases: `phase6Rig`, `"phase2-v1"`, `"phase2-single-shot"`, four runtime strings that cited "Phase 2/3".

**Why this priority**: it is the last place the claim "a feature can be lifted out" was false, and it was declared as such in the README rather than fixed.

**Independent Test**: `go test -tags=integration ./tests/integration -run 'TestMigrations_Moving|TestSpawn_RecordsLineage|TestFork_Records'` passes; the migration test brings a database to the old shape, migrates it forward and fails if `sessions` has a column outside an exact list or any table but `events` has a foreign key to it. `go test ./tests/contract -run 'TestNoBuildStageNames'` passes.

**Acceptance Scenarios**:

1. **Given** a database at the old schema holding a root session, a delegated child, a team member, a fork and a plan-pinned run, **When** migrations 0026 to 0029 run, **Then** each fact is in its feature's table, the sessions list's old filter and its new one pick the same rows, and `sessions` has none of the moved columns.
2. **Given** a session nobody spawned, **When** a delegation counts the delegations under it, **Then** its own delegations are counted even though it has no lineage record (it is its own root at depth 0).
3. **Given** `Executor.Start` for a plan, **When** it appends the first event, **Then** it pins the plan version to the session in the same transaction, so no caller names the plan on the session it creates.
4. **Given** a file name, a Go identifier or a Go string literal containing "phase", **When** the contract tests run, **Then** they fail; a `Phase N` comment is not checked.

### Key Entities

- **Feature package**: a package under `internal/` (or `kernel/`) that is not an adapter. Holds a feature's vocabulary, its use case and the ports it needs.
- **Adapter**: a package whose last path element is `postgres`, `redis`, `inmem`, `sealed`, `oauth2`, `otlp`, `langfuse`, `profiling`, `exechook`, `httphook`, `kerneladapter`, a provider name, `wire` or a test kit. Touches the outside world, or stands in for it.
- **Binding**: a feature's driving adapter, `<feature>/rest` (HTTP, mounted on the kit in `internal/surfaces/rest`) or `<feature>/tool` (the model). Imports its feature directly; governed by `allowedFeatureImports` and the no-storage rules rather than the adapter rule.
- **Table owner**: the one feature whose package tree may write SQL naming a table (`tests/contract/ownership_test.go`).
- **Persistence kit**: `internal/store`, `audit/postgres`, `crypto/postgres`, `eventlog/sealed`. The one way every feature's adapter reaches its tenant-scoped, sealed, receipted transaction.
- **Session aggregate**: the `sessions` table and `eventlog.Session`: a session's identity, routing, run configuration, status projection and who spawned it. It carries nothing a single feature stamps, and no feature table has a foreign key into it.
- **Spawn tree**: which session spawned which, how deep, under which root. Only `parent_session_id` is on the aggregate; depth and root are delegate's `session_lineage`, with no row for a session nobody spawned.
- **Settled observer**: a feature that cares what became of a session it does not run (`runctl.SettledObserver`).

## Success Criteria *(mandatory)*

- **SC-001**: `go list -deps ./kernel` lists none of the packages named in FR-002.
- **SC-002**: `go build ./... && go vet ./... && golangci-lint run --build-tags=integration ./... && go test -race -tags=integration ./...` passes, and no test asserts anything different. What the moves force are import paths, constructor arguments, and the setup of three tests: the pipeline's rewrite-hook test drives a fake `hooks.Handler` instead of shelling out to `/bin/sh`, the Phase 7 outbox test opens its delivery through the ledger instead of a `store` function, and the inbound-dedup test moved with its code.
- **SC-003**: Each of the new contract rules fails when a violation is injected, and passes when it is removed.
- **SC-004**: `cd web && npx tsc -b && npm run lint && npm run build` passes on the feature layout, and `go test -race -tags=integration ./...` passes with the integration tests renamed and their function names unchanged.
- **SC-005**: The migration test passes on a database brought to the old shape and migrated forward, and fails for an injected extra column, for a foreign key left behind, and for a `sessions` query added back to `delegate` or `teams` (`TestTableOwnership_SQLStaysInTheOwner`).

## Assumptions

- Nothing is deployed, so each move is a straight refactor with no data migration and no compatibility shim (see Phase 19 in spec 002).
- `sessions` and `events` are the shared aggregate root. The first two passes declared that every feature table had a foreign key to `sessions` and a few features stamped columns on it, and bounded the coupling (FR-009 lists every accessor). The third pass removed it (FR-014). Two columns stay on purpose: `autonomy_level` and `conversational` are `kernel.RunConfig` fields, pinned at creation and read by oversight, plan, delegate and runctl when they resume a run; moving them into one feature's table would make four others depend on it.
- The migrations behind FR-014 (0026 to 0029) are not expand/contract (`.claude/rules/migrations.md`): each creates, backfills and drops in one transaction, because nothing is deployed. Once something is, a change of this kind has to be split across releases.
