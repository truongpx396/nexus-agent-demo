---

description: "Task list for the OpenAPI contract"
---

# Tasks: OpenAPI contract

**Input**: [spec.md](spec.md), [plan.md](plan.md)

**Paths**: verified at HEAD (`tests/contract/specs_test.go` fails if a path named by a `[X]` task does not exist).

## Format: `- [X] T### [US#] Description in path (post-ledger)`

`(post-ledger)`: new work, after the build ledger closed; it has no `(bp N.M)` id.

## Phase 1: User Story 1 - the routes come from the spec

- [X] T001 [US1] Describe every route of the REST kit and the three probes in `api/openapi.yaml`, tagged by the feature that serves them (post-ledger; FR-001)
- [X] T002 [US1] Pin oapi-codegen as a `tool` in `go.mod`, add the `generate` target to `Makefile`, and give each feature's `rest` package a generator config, `internal/runs/rest/oapi-codegen.yaml`, `internal/runctl/rest/oapi-codegen.yaml`, `internal/oversight/rest/oapi-codegen.yaml`, `internal/contentaccess/rest/oapi-codegen.yaml`, and its committed output, `internal/runs/rest/api.gen.go`, `internal/runctl/rest/api.gen.go`, `internal/oversight/rest/api.gen.go`, `internal/contentaccess/rest/api.gen.go` (post-ledger; FR-002)
- [X] T003 [US1] The kit mounts a generated server: `Router.Authenticated` (the bearer-token check and the body bound on a route group) and `ParamError` in `internal/surfaces/rest/server.go`, tested in `internal/surfaces/rest/authenticated_test.go` (post-ledger; FR-003)
- [X] T004 [US1] The run endpoints implement the generated interface and answer with its models, in `internal/runs/rest/handler.go`, `internal/runs/rest/run_create.go`, `internal/runs/rest/run_get.go`, `internal/runs/rest/run_events.go`, `internal/runs/rest/sessions_list.go`; the live-preview message on the event bus is the spec's `RunDelta`, `internal/runs/rest/broker.go` (post-ledger; FR-003)
- [X] T005 [US1] The approval, run-control and content-access endpoints likewise, in `internal/oversight/rest/handler.go`, `internal/runctl/rest/handler.go`, `internal/contentaccess/rest/handler.go`; the content-access success paths, which had no test, get one in `internal/contentaccess/rest/handler_test.go` (post-ledger; FR-003, FR-006)

## Phase 2: User Story 2 - a stale or divergent spec fails the build

- [X] T006 [US2] `Server.Routes` lists the routes a server mounts, and `restModules` names the REST modules once, in `internal/surfaces/rest/server.go` and `cmd/nexusd/compose.go` (post-ledger; FR-004)
- [X] T007 [US2] Contract test that the spec is valid, describes exactly the routes the composed modules mount, declares authentication where the kit requires it, and that every described route answers `401` without a token, in `cmd/nexusd/api_contract_test.go` (post-ledger; FR-004, SC-003)
- [X] T008 [US2] The `build` job fails when `go generate ./...` leaves a diff, in `.github/workflows/ci.yml` (post-ledger; FR-005)

## Phase 3: Documentation

- [X] T009 [US1] `CLAUDE.md` says where a route is added and how the spec is checked; `specs/001-agent-platform/contracts/rest-api.md` points at the spec; `bruno/approvals/grant-approval-modified.bru` asserted `200` for a route that answers `202` (post-ledger)

## Open follow-ups

- [ ] T010 [US1] `nexusctl approvals grant` and `approvals deny` treat `202` as a failure (`internal/surfaces/cli/cli.go` checks for `200`), though the server has always answered `202`: the grant is recorded and the command reports an error. Fix the check, or generate the CLI's client from the spec so it cannot drift (post-ledger)
- [ ] T011 [US1] The web app's request and response types (`web/src/features/chat/types.ts`) are hand-written; generate them from `api/openapi.yaml` (post-ledger)
