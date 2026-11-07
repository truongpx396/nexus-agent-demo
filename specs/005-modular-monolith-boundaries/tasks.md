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

## Open follow-ups

Found while doing the above and deliberately left, each because it is a decision or a redesign rather than a move.

- [ ] T018 [US3] Route the operator commands' SQL (`cmd/nexusd/cli_seed.go` inserting tenants, `cli_golive.go` counting budgets, `cli_erase.go` looking up a session's tenant) through the owning ports, and drop the `cli_*.go` exemption from `tests/contract/composition_test.go`
- [ ] T019 [US1] Split `internal/connectors` into the vault service and an OAuth2 adapter so its public API stops carrying `golang.org/x/oauth2` types, and add it to the no-infrastructure list (`internal/connectors/oauth.go`)
- [ ] T020 [US1] Remove the unsandboxed `os/exec` fallback from `internal/skills/script_tool.go` (it exists for tests) and add `skills` to the no-infrastructure list
- [ ] T021 [US4] The event log's hooks take a raw `pgx.Tx` and are wired in `cmd/nexusd/ports.go` (`internal/eventlog/postgres/log.go`), while every other service uses a port-typed unit of work. Unify them. [NEEDS CLARIFICATION: whether the audit receipt, the approval row and the delegation bind should all become `eventlog.Appender`-style units, which changes how a suspension commits.]
- [ ] T022 [US4] Decide whether a feature should register its own event types instead of adding to the one closed list in `internal/eventlog/event.go`. [NEEDS CLARIFICATION: the closed list is what lets the log be hash-chained and upcast as one; an open registry buys feature self-containment at the cost of that guarantee.]
- [ ] T023 [US4] Decide whether each feature's REST handlers should sit beside the feature (`oversight/rest`) or stay grouped by transport in `internal/surfaces/rest`. [NEEDS CLARIFICATION: the current grouping is intentional (`surfaces/` is the driving side), so this is a choice, not a defect.]
