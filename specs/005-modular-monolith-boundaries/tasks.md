---

description: "Task list for modular-monolith boundaries"
---

# Tasks: Modular-monolith boundaries

**Input**: [spec.md](spec.md), [plan.md](plan.md)

**Paths**: verified at HEAD (`tests/contract/specs_test.go` fails if a path named by a `[X]` task does not exist).

## Format: `- [X] T### [US#] Description in path (post-ledger)`

`(post-ledger)`: new work, after the build ledger closed; it has no `(bp N.M)` id.

## Phase 1: User Story 1 - the core reaches no outside world

- [X] T001 [US1] Move the adapter for the kernel's own `ToolExecutor` port out of the kernel, so the kernel no longer imports the tool pipeline, to `internal/tools/kerneladapter/executor.go` (post-ledger; FR-001)
- [X] T002 [US1] Move the OTLP and Langfuse exporters and the pprof and Pyroscope clients out of the telemetry vocabulary into `internal/obs/otlp/exporter.go`, `internal/obs/langfuse/exporter.go`, `internal/obs/profiling/pprof.go`, `internal/obs/profiling/pyroscope.go`; `Attrs.SumInt` moves to `internal/obs/allowlist.go` (post-ledger; FR-002)
- [X] T003 [US1] Split the hook port from its handlers: the JSON wire protocol in `internal/hooks/wire.go`, the command handler in `internal/hooks/exechook/handler.go`, the http handler in `internal/hooks/httphook/handler.go`; `NewDispatcher` registers only the prompt kind and `cmd/nexusd/pipeline.go` registers the others (post-ledger; FR-002)
- [X] T004 [US1] Content-access grants and the golden-signal, usage and spend reports were business features inside the telemetry package; they become `internal/contentaccess/grants.go`, `internal/contentaccess/postgres/grants.go`, `internal/reporting/reports.go`, `internal/reporting/golden_signals.go`, `internal/reporting/usage.go`, `internal/reporting/postgres/golden_signals.go` (post-ledger; FR-002)
- [X] T005 [US1] Forbid `internal/tools` in the kernel and add the no-infrastructure ratchet for 28 packages, in `tests/contract/boundaries_test.go` (post-ledger; FR-001, FR-002)

## Phase 2: User Story 2 - every table has an owner

- [X] T006 [US2] Claims and checkpoints move from `internal/store` into the runctl adapter, using `runctl.Claim`; the checkpoint type and its writer are `internal/runctl/checkpoint.go`, `internal/runctl/postgres/claims.go`, `internal/runctl/postgres/checkpoint.go` (post-ledger; FR-003)
- [X] T007 [US2] Deliveries and inbound-delivery dedup move into the surfaces adapter, using `surfaces.DeliveryStatus`, in `internal/surfaces/postgres/delivery_sql.go`, `internal/surfaces/postgres/inbound_sql.go` (post-ledger; FR-003)
- [X] T008 [US2] The messaging-channel adapter leaves `package main` for `internal/surfaces/postgres/channels.go` (returning its own SMTP settings so the shared adapter imports no one surface), and the derived-artifact recorder becomes `internal/tools/postgres/derived_artifacts.go` (post-ledger; FR-003)
- [X] T009 [US2] The tenants table gets an owner: `tenancy.Directory` and `tenancy.ForEach` in `internal/tenancy/tenancy.go`, the operator-role adapter in `internal/tenancy/postgres/directory.go` (post-ledger; FR-003)
- [X] T010 [US2] The anchor-and-verify pass and the control plane's receipt backend use a chain bound to a store, `internal/audit/postgres/scoped.go` (post-ledger; FR-003)

## Phase 3: User Story 3 - the composition root wires

- [X] T011 [US3] The post-run checkpoint, the idle-conversation sweep and orphan recovery move into runctl as `Control.SaveCheckpoint`, `Control.SweepIdle`, `runctl.StartRecovery`, with the cross-tenant finder in `internal/runctl/postgres/orphans.go`; `cmd/nexusd/background.go` keeps only the schedule; tests in `internal/runctl/lifecycle_test.go`, `internal/runctl/recovery_test.go` (post-ledger; FR-004, FR-007)
- [X] T012 [US3] "Tell the delegation and the team a session settled" becomes `runctl.SettledObserver` and `Control.NotifySettled` in `internal/runctl/settled.go`, implemented by `internal/delegate/resolve.go` and `internal/teams/lifecycle.go`, registered once in `cmd/nexusd/serve.go` and used by `cmd/nexusd/ports.go` (post-ledger; FR-007)
- [X] T013 [US3] Contract test that the serving files of `cmd/nexusd` hold no SQL, with the operator commands as the one named exemption, in `tests/contract/composition_test.go` (post-ledger; FR-004)

## Phase 4: User Story 4 - features meet through ports

- [X] T014 [US4] Invert the two cross-feature adapter edges: the budget writer injected into `internal/runs/postgres/sessions.go` and the envelope writer injected into `internal/plan/postgres/store.go` (post-ledger; FR-005)
- [X] T015 [US4] Consumer-side interfaces replace the concrete service pointers: `ApprovalOpener`, `InputRequester`, `Delegator` in `internal/plan/ports.go`, `ApprovalInvalidator`, `InputInvalidator` in `internal/runctl/types.go`; a plan with an approval gate is tested against a fake in `internal/plan/exec_test.go` (post-ledger; FR-005)
- [X] T016 [US4] Adapter isolation and the declared feature-dependency table, in `tests/contract/modules_test.go` (post-ledger; FR-005, FR-006)

## Phase 5: documentation

- [X] T017 Declare the shared kernel, the read model and the named exceptions, and describe the four rules, in `README.md` and `CLAUDE.md` (post-ledger; FR-008)

## Phase 6: User Story 5 - data ownership is enforced

- [X] T018 [US5] The operator commands' SQL goes through the owning ports (`tenancy.Register`, `tenancy.SessionLocator`, the cost adapter's `GetBudget`), so `cmd/nexusd` holds no SQL at all and the `cli_*.go` exemption is gone, in `internal/tenancy/tenancy.go`, `internal/tenancy/postgres/directory.go`, `internal/tenancy/postgres/register.go`, `cmd/nexusd/cli_seed.go`, `cmd/nexusd/cli_golive.go`, `cmd/nexusd/cli_erase.go`, `tests/contract/composition_test.go` (post-ledger; FR-004)
- [X] T024 [US5] Erasure no longer writes the tool pipeline's table: `cryptopg.Eraser` takes an `ArtifactStore` that `tools/postgres` satisfies and `cmd/nexusd` injects, so the artifacts still go in the same transaction as the key shred, and an Eraser with none fails closed (before it touches the transaction); the reconcile backstop is now covered by a test that closes a gap end to end, which found that its query had always compared a `uuid` to `text` and so had never been able to run, in `internal/crypto/postgres/erase.go`, `internal/crypto/postgres/erase_test.go`, `internal/crypto/postgres/shred.go`, `internal/tools/postgres/derived_artifacts.go`, `tests/integration/oversight_test.go` (post-ledger; FR-005, FR-009)
- [X] T025 [US5] The `runctl` session queries and the `eventlog` snapshot cache leave `internal/store`, which keeps only the session/event aggregate, tenant scoping and migrations, in `internal/runctl/postgres/awaiting.go`, `internal/eventlog/postgres/snapshots.go`, `internal/store/session.go` (post-ledger; FR-003)
- [X] T026 [US5] Contract test that every migrated table has an owner, that SQL naming a table stays in the owner's package tree, that the `sessions`/`events` kernel tables list each accessor with a reason, and that the `reporting` read model only reads, in `tests/contract/ownership_test.go` (post-ledger; FR-009)

## Phase 7: User Story 6 - a feature's driving adapters sit beside it

- [X] T027 [US6] The REST kit: the server, bearer-token auth on every route (a `Module` registers a route only through `Router.Handle`), CORS, `Principal` and `WriteJSON`, with no endpoint and no feature import, plus the `resttest` test kit, in `internal/surfaces/rest/server.go`, `internal/surfaces/rest/authn.go`, `internal/surfaces/rest/capability.go`, `internal/surfaces/rest/server_test.go`, `internal/surfaces/rest/capability_test.go`, `internal/surfaces/rest/resttest/resttest.go` (post-ledger; FR-010)
- [X] T028 [US6] The run endpoints, the SSE broker and the event bus move beside the `runs` feature, in `internal/runs/rest/handler.go`, `internal/runs/rest/run_create.go`, `internal/runs/rest/run_get.go`, `internal/runs/rest/run_events.go`, `internal/runs/rest/sessions_list.go`, `internal/runs/rest/broker.go`, `internal/runs/rest/reads_test.go`, `internal/runs/rest/broker_test.go` (post-ledger; FR-010)
- [X] T029 [US6] The approval endpoints call the feature directly, and the drain-then-notify logic that lived in a `cmd/nexusd` translation struct becomes `oversight.Decisions`, with unit tests, in `internal/oversight/decide.go`, `internal/oversight/decide_test.go`, `internal/oversight/rest/handler.go`, `internal/oversight/rest/handler_test.go` (post-ledger; FR-010)
- [X] T030 [US6] The run-control endpoints call `runctl` directly, and "continue a paused conversation, otherwise steer" becomes `Control.Send` instead of a status read in the handler, in `internal/runctl/steer.go`, `internal/runctl/control_test.go`, `internal/runctl/rest/handler.go`, `internal/runctl/rest/handler_test.go` (post-ledger; FR-010)
- [X] T031 [US6] The content-access grant endpoints move beside `contentaccess`, in `internal/contentaccess/rest/handler.go` (post-ledger; FR-010)
- [X] T032 [US6] The board, delegate and retrieve tools move into their features, so the three translation structs in `cmd/nexusd` go; the delegate tool uses `delegate.SpawnRequest` and `delegate.MaxDepth/MaxConcurrent/MaxPerRun` instead of copies, in `internal/teams/tool/board.go`, `internal/teams/tool/adapter.go`, `internal/delegate/tool/delegate.go`, `internal/retrieval/tool/retrieve.go`, `cmd/nexusd/pipeline.go`, `cmd/nexusd/ports.go` (post-ledger; FR-010)
- [X] T033 [US6] Each binding declares its imports in `allowedFeatureImports`, may not import storage or the kernel directly (the run and grant bindings may not reach it at all), and joins the no-infrastructure list, in `tests/contract/modules_test.go`, `tests/contract/boundaries_test.go` (post-ledger; FR-006, FR-010)

## Phase 8: User Story 7 - the remaining infrastructure leaks and layer-shaped packages

- [X] T019 [US7] Split `internal/connectors` into the vault and an OAuth2 adapter, so its API stops carrying `golang.org/x/oauth2` types: `connectors.Token` and the `Authorizer` port in the core, the protocol in the adapter, and `connectors` joins the no-infrastructure list, in `internal/connectors/ports.go`, `internal/connectors/oauth.go`, `internal/connectors/oauth2/registry.go`, `internal/connectors/oauth2/registry_test.go`, `cmd/nexusd/pipeline.go` (post-ledger; FR-002, FR-011)
- [X] T020 [US7] Remove the unsandboxed `os/exec` fallback from the skill script tool: with no sandbox the call is refused (`skills.ReasonNoSandbox`), `skills` joins the no-infrastructure list, in `internal/skills/script_tool.go`, `internal/skills/script_tool_test.go` (post-ledger; FR-002, FR-011)
- [X] T021 [US7] The event log's suspend and delegate hooks are callbacks over the kernel's request types with no `pgx.Tx` (the kernel calls them after the append, in their own transactions), the delegation bind is `Delegations.BindToolUse`, and the audit chain, the one thing that shares the append's transaction, is a nil-valid `Hooks.Chain`, in `internal/eventlog/postgres/log.go`, `internal/delegate/delegations.go`, `internal/delegate/postgres/store.go`, `internal/delegate/delegations_test.go`, `cmd/nexusd/ports.go` (post-ledger; FR-012)
- [X] T022 [US7] The event taxonomy stays one closed list (the kernel authors events from most features' vocabularies, so feature-owned types would force `kernel` -> feature imports), with every type owned by a named package, a test that keeps the table and the constants in step, and `Append` refusing a type outside it, in `internal/eventlog/taxonomy.go`, `internal/eventlog/taxonomy_test.go`, `internal/eventlog/event.go`, `internal/eventlog/logtest/contract.go`, `internal/store/append.go`, `internal/eventlog/inmem/log.go` (post-ledger; FR-012)
- [X] T023 [US7] REST handlers sit beside the feature rather than grouped by transport: decided in favour, and done as T027 to T031 (post-ledger; FR-010)
- [X] T034 [US7] The web app is packaged by feature, not by kind of file: chat and approvals as features and the REST client, settings and generic UI as the shared part, with the one `api.ts` and `types.ts` split by feature, in `web/src/features/chat/api.ts`, `web/src/features/chat/types.ts`, `web/src/features/chat/ChatThread.tsx`, `web/src/features/approvals/api.ts`, `web/src/features/approvals/types.ts`, `web/src/features/approvals/Approvals.tsx`, `web/src/shared/api/client.ts`, `web/src/shared/settings/settings.tsx`, `web/src/shared/ui/JsonView.tsx` (post-ledger; FR-013)
- [X] T035 [US7] The integration tests and one `cmd/nexusd` file are named for the feature they exercise, not the build phase, in `tests/integration/oversight_test.go`, `tests/integration/reliability_test.go`, `tests/integration/harness_growth_test.go`, `tests/integration/orchestration_test.go`, `tests/integration/teams_test.go`, `tests/integration/reporting_dashboard_test.go`, `tests/integration/evalgate_test.go`, `tests/integration/connectors_secret_leak_test.go`, `tests/integration/retrieval_test.go`, `tests/integration/contentaccess_grants_test.go`, `tests/integration/reporting_reports_test.go`, `cmd/nexusd/surfaces_wiring.go` (post-ledger; FR-013)
- [X] T036 [US7] Document the five rules, the driving-adapter layout, the closed taxonomy and what remains shared, in `README.md`, `CLAUDE.md`, `web/README.md` (post-ledger; FR-008)

## Phase 9: User Story 8 - the session aggregate carries only its own columns

- [X] T037 [US8] Move what features stamp on `sessions` into feature-owned tables and drop the foreign keys from feature tables to it: `team_members` (teams), `session_lineage` (delegate, with teams reading and writing it through functions `cmd/nexusd` injects), `session_forks` (runctl), `plan_sessions` (plan, pinned by `Executor.Start`), then every remaining foreign key, in `migrations/0026_team_members.sql`, `migrations/0027_session_lineage.sql`, `migrations/0028_fork_and_plan_pins.sql`, `migrations/0029_features_reference_sessions_by_value.sql`, `internal/eventlog/session.go`, `internal/store/session.go`, `internal/teams/ports.go`, `internal/teams/lifecycle.go`, `internal/teams/postgres/roster.go`, `internal/teams/postgres/lineage.go`, `internal/delegate/lineage.go`, `internal/delegate/ports.go`, `internal/delegate/spawn.go`, `internal/delegate/delegations.go`, `internal/delegate/postgres/lineage.go`, `internal/delegate/postgres/store.go`, `internal/runctl/fork.go`, `internal/runctl/postgres/forks.go`, `internal/plan/exec.go`, `internal/plan/postgres/pins.go`, `cmd/nexusd/serve.go` (post-ledger; FR-014)
- [X] T039 [US8] The aggregate's shape is pinned against the real schema: a database is brought to the old shape, migrated forward and checked fact by fact, and `sessions` must have exactly `aggregateColumns` and no foreign key from anything but `events`; `delegate` and `teams` leave the `sessions` accessor list, in `tests/integration/session_migration_test.go`, `tests/integration/session_aggregate_test.go`, `tests/contract/ownership_test.go` (post-ledger; FR-009, FR-014)
- [X] T040 [US8] No name carries a build phase: `reliabilityRig` for the reliability tests' rig, one named pair of digest defaults for the three copies of `"phase2-v1"`, four runtime strings that cited a phase reworded, and a rule that fails on a file name, identifier or string literal containing "phase", in `internal/harness/digest.go`, `internal/runs/launch.go`, `internal/runctl/fork.go`, `internal/surfaces/cron/scheduler.go`, `tests/integration/reliability_test.go`, `tests/contract/naming_test.go` (post-ledger; FR-015)

## Open follow-ups

Left deliberately, because it is a tooling decision rather than a move.

- [ ] T038 [US7] Split `web/src/index.css` by feature. [NEEDS CLARIFICATION: several classes are composed at runtime (`status-*`) and some rules are dead, so a split cannot be checked by reading; it needs visual regression tests the repository does not have.]
