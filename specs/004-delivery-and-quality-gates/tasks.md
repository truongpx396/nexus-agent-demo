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

## Phase 6: User Story 6 - the pipeline cannot hang, drift or leak goroutines unnoticed

- [X] T011 [US6] Give every CI job a `timeout-minutes` sized from the last 60 runs (at least 2.5 times the longest healthy run, never under five minutes), run `ci.yml` with `contents: read`, cancel a PR's superseded run (never a push to `main`), and pin every `uses:` to a commit, in `.github/workflows/ci.yml`, `.github/workflows/govulncheck.yml`, `.github/workflows/pr-size.yml`, `.github/workflows/spec-gate.yml`
- [X] T012 [US6] Fail `build` on a `go.mod` or `go.sum` that `go mod tidy -diff` would change or a module that `go mod verify` rejects, and run the unit job with `-shuffle=on`, in `.github/workflows/ci.yml`
- [X] T013 [US6] Group Dependabot's minor and patch bumps into one PR per ecosystem and hold a release for seven days before proposing it, in `.github/dependabot.yml`; make `make lint`'s help name the real pin source instead of a stale version, in `Makefile`
- [X] T014 [US6] Run goleak after the tests of every package that starts a goroutine and has tests (16 packages, including the kernel and the integration suite), in `kernel/leak_test.go`, `internal/queue/leak_test.go`, `internal/runctl/leak_test.go`, `internal/delegate/leak_test.go`, `internal/teams/leak_test.go`, `tests/integration/leak_test.go`, and one in each of the other ten
- [X] T015 [US6] Fail the build when such a package has no goleak `TestMain`, with a fixture test that proves the scanner flags each way of missing it and an empty exemption list, in `tests/contract/goleak_test.go`; close the two leaks it found, a stream a test never drained and a Docker client a test never closed, in `internal/provider/anthropic/anthropic_test.go`, `internal/tools/builtin/shell_sandbox_integration_test.go`
- [ ] T016 [US6] Repository settings that no file can carry: a ruleset on `main` that requires the CI checks (`lint`, `unit`, `integration`, `eval-gate`, `spec-gate`, `pr-size`) and blocks force-push and deletion, and Dependabot security updates turned on

## Phase 7: User Story 7 - the Go code follows current idiom, fails closed, and the linter keeps it there

- [X] T017 [US7] Finish the graceful shutdown before `serve` returns, make `startPeriodic` safe to stop more than once and wait for its pass, await the queue workers before their pool closes, reject an unknown `nexusd` subcommand and cancel long subcommands on SIGINT/SIGTERM, and bound each `signerd` connection and drain in-flight signs on shutdown, in `cmd/nexusd/serve.go`, `cmd/nexusd/background.go`, `cmd/nexusd/main.go`, `cmd/signerd/main.go`, `cmd/nexusd/background_test.go`
- [X] T018 [US7] Keep infrastructure error text out of response bodies: add `rest.ServerError` (log the cause, answer `<op>: internal error`) and use it in the run and approval handlers, answer the webhooks' channel lookup and dispatch failures and the `/readyz` and `/metrics` probes the same way, in `internal/surfaces/rest/errors.go`, `internal/surfaces/rest/errors_test.go`, `internal/runs/rest/run_create.go`, `internal/runs/rest/run_get.go`, `internal/runs/rest/sessions_list.go`, `internal/oversight/rest/handler.go`, `internal/surfaces/telegram/webhook.go`, `internal/surfaces/zalo/webhook.go`, `internal/surfaces/email/webhook.go`, `cmd/nexusd/http.go`
- [X] T019 [US7] Make `Kernel.Resume` and `ResumeDelegation` fail closed on an unrecognised resolution, and drop the `runTurns` start-turn parameter and the `condense` error result that never varied, in `kernel/loop.go`, `kernel/turns.go`, `kernel/metering.go`, `kernel/resume_test.go`
- [X] T020 [US7] Match errors with `errors.Is` and `errors.AsType`, name the four error types `XxxError` (`ApprovalNotPendingError`, `UnresolvedClaimsError`, `UnknownTypeError`, `EnvironmentMismatchError`), and stop shadowing `max`, in `internal/oversight/approval.go`, `internal/runctl/resume.go`, `internal/eventlog/taxonomy.go`, `evals/environment.go`, `internal/audit/postgres/chain.go`, `internal/cost/postgres/budget.go`, `internal/queue/redis/streams.go`, `internal/plan/validate.go`
- [X] T021 [US7] Adopt the standard library's newer forms (`slices`, `maps`, `strings.SplitSeq`, `errors.New`, range over int), seal in one allocation, cap what the sandbox reads back from a container, and enforce all of it with `errorlint`, `errname`, `copyloopvar`, `intrange`, `modernize`, `perfsprint`, `nolintlint`, `predeclared`, `unconvert` and `usestdlibvars`, in `.golangci.yml`, `internal/crypto/envelope.go`, `internal/sandbox/sandbox.go`, `internal/tools/identity.go`, `internal/config/config.go`
- [X] T022 [US7] Remove the 45 MB macOS `nexusd` binary that #114 committed at the repository root and ignore a root-level `nexusd`, `nexusctl` or `signerd` (what `go build ./cmd/<name>` writes), in `.gitignore`
- [X] T023 [US7] Give the refusals of run control, content access and approval lookup sentinels (`eventlog.ErrSessionNotFound`, `runctl.ErrSessionState`, `runctl.ErrInvalidArgument`, `contentaccess.ErrNoGrant`) in `internal/eventlog/session.go`, `internal/runctl/errors.go`, `internal/runctl/steer.go`, `internal/runctl/converse.go`, `internal/runctl/resume.go`, `internal/runctl/autonomy.go`, `internal/runctl/fork.go`, `internal/contentaccess/grants.go`, and map them in `internal/runctl/rest/handler.go`, `internal/contentaccess/rest/handler.go`, `internal/oversight/rest/handler.go` so a storage failure is a logged 500
- [X] T024 [US7] Bound every request body at 4 MiB in `Router.Handle` and add `rest.DecodeJSON` (413 past the limit, 400 for malformed) in `internal/surfaces/rest/server.go`, used by `internal/runs/rest/run_create.go` and the run-control, approval and grant handlers
- [X] T025 [US7] Cap each PDF stream's inflation at 32 MiB and a document's at 256 MiB, skipping a stream past either, in `internal/ingest/convert.go`
- [X] T026 [US7] Replace `http.DefaultClient` in the model providers with a client that bounds the wait for response headers (90 s Anthropic, 5 min LiteLLM) and not the stream, in `internal/provider/anthropic/anthropic.go`, `internal/provider/litellm/litellm.go`
- [X] T027 [US7] Split `serve` into the steps its phases are (`openInfra` and `startProfiling` in `cmd/nexusd/infra.go`, `buildServices`, `newRESTAPI` and `publishDeltas` in `cmd/nexusd/compose.go`, `startBackground` and `startScheduler` in `cmd/nexusd/background.go`, `newMux` in `cmd/nexusd/surfaces_wiring.go`, `runHTTPServer` in `cmd/nexusd/http.go`), return `newToolPipeline`'s errors instead of exiting, close the Redis client, stop and await the cron scheduler, and replace `main`'s `switch` with a command table whose usage text is derived from it, in `cmd/nexusd/serve.go`, `cmd/nexusd/main.go`, `cmd/nexusd/pipeline.go`
- [X] T028 [US7] Replace package `sort` with `slices` and `cmp` throughout (`internal/cost/pricebook.go`, `internal/memory/store.go`, `internal/promptctx/builder.go`, `internal/skills/manifest.go`, `internal/skills/digest.go`, `kernel/hygiene.go` and the rest), ban it in `.golangci.yml`, enable `modernize`'s `omitzero` by dropping the `omitempty` that never applied to `plan.Predicate.Value` (the JSON bytes are unchanged and pinned in `internal/plan/predicate_test.go`), and give `GateResult` one receiver kind in `evals/baseline.go`
