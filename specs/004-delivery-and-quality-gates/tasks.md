---

description: "Task list for delivery and quality gates (proposed)"
---

# Tasks: Delivery and quality gates

**Input**: [spec.md](spec.md), [plan.md](plan.md)

**Paths**: verified at HEAD (`tests/contract/specs_test.go` checks the path in every `[X]` task; these are all open, so the paths named are the intended locations).

> **Moved here from spec 001.** These tasks were T239–T243 in the Convergence backlog of [spec 001](../001-agent-platform/tasks.md), which `/speckit-converge` appended against `8e02491`. Each is re-checked against the code on 2026-11-02 and still open. The backlog ids are kept in brackets so history stays traceable.

## Format: `- [ ] T### [US#] Description in path (was 001 T###)`

## Phase 1: User Story 1 - durability is defined and rehearsed

- [ ] T001 [US1] Define backup, restore, RPO and RTO for the event log and audit chain, including custody of the KEK and the audit signer's key, in a new `docs/durability.md` (was 001 T240)
- [ ] T002 [US1] Add a restore-rehearsal item to the go-live checklist, printed as a manual reminder, in `docs/go-live.md`, `cmd/nexusd/cli_golive.go` (was 001 T240)
- [ ] T003 [US1] Automate a restore rehearsal against a disposable Postgres: restore, then `nexusd verify-chain` and a projection replay, in `tests/integration/restore_test.go` (was 001 T240)

## Phase 2: User Story 2 - service levels

- [ ] T004 [US2] Decide which service levels to state, then define their SLOs and error-budget policy in `docs/observability.md` (was 001 T241)
- [ ] T005 [US2] Replace the uncalibrated thresholds with burn-rate alerts that each name a runbook, in `deploy/observability/prometheus-rules.yml`, and write the runbooks in a new `docs/runbooks/` (was 001 T241)

## Phase 3: User Story 3 - SBOM and signed artifacts

- [ ] T006 [US3] Decide what a release is, and add a publish step for the container images in `.github/workflows/ci.yml` (was 001 T239)
- [ ] T007 [US3] Generate an SBOM and sign the published images in `.github/workflows/ci.yml` (was 001 T239)

## Phase 4: User Story 4 - release-gate metrics

- [ ] T008 [US4] Report quality-per-dollar and completions-per-million-tokens in every release gate, marked not applicable on the scripted corpus, in `evals/efficiency.go`, `evals/gate.go`, `evals/types.go` (was 001 T242)

## Phase 5: User Story 5 - routing capability floor

- [ ] T009 [US5] Add a capability floor for feature demand to deterministic routing and record it in the routing reason, in `internal/provider/router.go` (was 001 T243)
- [ ] T010 [US5] Add a table-driven test of the floor in `internal/provider/router_test.go` (was 001 T243)
