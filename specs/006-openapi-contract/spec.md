# Feature Specification: OpenAPI contract

**Feature Branch**: `006-openapi-contract`

**Created**: 2026-11-11

**Status**: Implemented. Two follow-ups stay open on purpose (see [tasks.md](tasks.md#open-follow-ups)).

**Input**: The REST API had no machine-readable description. Each route was a hand-written Gin handler with its own request and response structs, the prose contract (`specs/001-agent-platform/contracts/rest-api.md`) and the Bruno collection were kept in step by hand, and nothing checked either against the code. The drift was already there: one Bruno request asserted `200` for a route that answers `202`, and `nexusctl approvals grant` and `deny` treat `202` as a failure.

This adds `api/openapi.yaml` as the one description of the API and uses [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen) to make each feature's models and its handler interface from it, so a route exists because an operation in the spec does. It changes no route, no status code and no field name.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The routes come from the spec (Priority: P1)

**Current state (before)**: a route was `r.POST("/v1/runs/:id/cancel", h.handleCancelRun)`, the request and response were structs declared beside the handler, and the path parameter was parsed by hand in every handler (`uuid.Parse(c.Param("id"))`, repeated 11 times).

**Why this priority**: it is the claim "the spec is the contract", and a description that nothing holds the code to is documentation.

**Independent Test**: `make generate && git diff --exit-code` leaves nothing; `go test ./cmd/nexusd -run TestAPISpec` passes; deleting an operation from the spec (without regenerating) or adding a route by hand fails it.

**Acceptance Scenarios**:

1. **Given** an operation tagged `approvals` in `api/openapi.yaml`, **When** `make generate` runs, **Then** `internal/oversight/rest/api.gen.go` holds its request and response models and a method of `ServerInterface` for it, and the handler fails to compile until it implements the method.
2. **Given** a handler for `GET /v1/runs/{id}`, **When** a request carries an `id` that is not a uuid, **Then** it is answered `400` by the generated wrapper and the handler is not called.
3. **Given** a route mounted by a generated registration, **When** it is asked for with no token, **Then** it answers `401`, like every route of the kit; a body over 4 MiB is `413`; a path no module mounts is `404` without a token.

### User Story 2 - A stale or divergent spec fails the build (Priority: P1)

**Why this priority**: generated code is committed, so a spec edit that was not followed by `make generate` would leave the code describing a different API.

**Independent Test**: edit a schema in the spec and run the build job's check; it fails with the diff.

**Acceptance Scenarios**:

1. **Given** a spec edit with no regeneration, **When** CI's `build` job runs `go generate ./...`, **Then** `git diff --exit-code` fails.
2. **Given** an operation in the spec that no composed module mounts, or a route a module mounts that the spec does not describe, **When** `go test ./cmd/nexusd` runs, **Then** it fails and names the route.
3. **Given** the spec, **When** it is loaded and validated as an OpenAPI document, **Then** it is valid; an operation outside the `ops` tag that does not declare bearer authentication, or a probe that does, fails the test.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `api/openapi.yaml` (OpenAPI 3.0.3) MUST describe every route of the REST kit and the three probes. Each operation carries one tag: the feature that serves it (`runs`, `run-control`, `approvals`, `content-access`) or `ops` for what `nexusd` serves outside the kit (`/healthz`, `/readyz`, `/metrics`, with no code generated). The webhooks are out of scope: they authenticate with a per-tenant secret or HMAC, not a bearer token.
- **FR-002**: Each feature's `rest` package MUST generate its models and its `ServerInterface` from the operations of its own tag, into a committed `api.gen.go`, with `oapi-codegen.yaml` beside it and a `go:generate` line in its handler file, so no feature's generated code names another's. oapi-codegen is pinned as a `tool` in `go.mod`; `make generate` runs it.
- **FR-003**: A feature's `Routes` MUST mount its generated interface on `Router.Authenticated()`, which carries the same bearer-token check and body bound as `Router.GET` and `Router.POST`, and MUST pass `restkit.ParamError` as the generator's error handler so a bad parameter is answered as plain text like every other error. A handler implements the generated method and answers with the generated models.
- **FR-004**: `go test ./cmd/nexusd` MUST fail when the spec and the composed modules' routes differ in either direction, when the spec is not a valid OpenAPI document, or when an operation's `security` disagrees with whether the kit authenticates it.
- **FR-005**: CI MUST fail when the committed generated code differs from what the spec generates.
- **FR-006**: The change MUST NOT alter a route, a status code, a field name or a field's JSON type. The deliberate differences are the three in the Assumptions below.
- **FR-007**: The server generated is the plain Gin one, not the strict one. A handler keeps its `*gin.Context`: the principal, the SSE stream and the plain-text errors all ride on it, and the strict generator hands a handler the `*gin.Context` itself as its `context.Context`, which the kit's rule forbids (a handler passes `c.Request.Context()`, never `c`, to anything that waits).

### Key Entities

- **Spec**: `api/openapi.yaml`, the input of every generator config and of the contract test.
- **Operation tag**: names the feature that serves an operation and so which package generates it, or `ops` for none.
- **Generated interface**: `ServerInterface` in a feature's `api.gen.go`, one method per operation, with typed path parameters. The feature's `Handler` implements it.

## Success Criteria *(mandatory)*

- **SC-001**: `make generate` is idempotent, and `git diff --exit-code` after it is empty.
- **SC-002**: `go build ./... && go vet ./... && golangci-lint run --build-tags=integration ./... && go test ./...` passes, and the existing handler tests pass with only their type names changed (`ForkView` to `ForkRunResponse`, `StatusResuming` to `DecisionResponseStatusResuming`).
- **SC-003**: Each rule in FR-004 fails when a violation is injected (a renamed path in the spec, a probe that declares authentication) and passes when it is removed.

## Assumptions

- Nothing is deployed, so a change to an error message needs no compatibility shim (see spec 002, Phase 19).
- Three behaviours differ on purpose, all in the direction of the spec: a malformed path id answers the generator's message (`Invalid format for parameter id: ...`) instead of `invalid run id`; `GET .../content-access-grants/read` answers `[]` for an empty history instead of `null`; and a `grantee_id` that is present but not a uuid answers `invalid JSON body` (it fails to decode) instead of `"grantee_id" is invalid`, while an absent one answers `"grantee_id" is required`.
- `GET .../content-access-grants/read` encodes its events under capitalised keys (`EventID`, `Seq`, ...) because `contentaccess.EventDTO` has no JSON tags. The spec records that as it is (`GrantedEvent`) rather than changing the wire shape in a change that promises none.
- The web app's hand-written TypeScript types and `nexusctl`'s hand-written client are not generated from the spec. oapi-codegen produces Go; both are follow-ups.
- The generated files count toward the 1,000-line PR limit. They are reproducible from the spec and checked by CI, which is the case the limit's exclusions (lockfiles, `web/dist`) are for, but this change does not alter `scripts/pr-size.sh`.
