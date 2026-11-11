# Implementation Plan: OpenAPI contract

**Spec**: [spec.md](spec.md) | **Status**: Implemented; two follow-ups open

## Summary

Write the API down once, as `api/openapi.yaml`, and generate from it with oapi-codegen: per feature, the models and the `ServerInterface` its handler implements. The feature `rest` packages keep their boundaries (each generates only its own tag's operations), the kit keeps authentication and the body bound (a generated registration mounts on `Router.Authenticated()`), and a contract test and a CI check hold the spec, the code and the generated files together.

## Technical Context

- **Spec**: `api/openapi.yaml`; operations tagged `runs`, `run-control`, `approvals`, `content-access`, `ops`.
- **Generator**: `github.com/oapi-codegen/oapi-codegen/v2` v2.8.2, pinned by a `tool` line in `go.mod` (run as `go tool oapi-codegen`), so there is no second place its version lives and Dependabot bumps it with the rest. Its runtime (`github.com/oapi-codegen/runtime`, path-parameter binding and the uuid type) is a direct dependency of the four `rest` packages.
- **Per feature**: `internal/{runs,runctl,oversight,contentaccess}/rest/{oapi-codegen.yaml,api.gen.go}`; config is `gin-server` plus `models`, `always-prefix-enum-values` (an enum value is otherwise a bare package-level name such as `Internal` or `Content`) and the initialisms normalizer (`RunID`, not `RunId`).
- **Kit**: `internal/surfaces/rest/server.go` gains `Router.Authenticated`, `ParamError` and `Server.Routes`.
- **Composition**: `cmd/nexusd/compose.go` names its modules once in `restModules`, which the contract test composes too.
- **Enforcement**: `cmd/nexusd/api_contract_test.go`, and the `generated code is up to date` step of CI's `build` job.

## Constitution Check

- **Principle I (one loop, many surfaces)**: unchanged. A handler is still a thin translator over its feature's use case; the generated interface only types its inputs.
- **Principle VI (tenant first)**: unchanged. The principal still comes only from the verified token the kit stored; a generated route is mounted behind the same `authenticate`, and a test asks every described route for `401`.
- **Principle IX (govern every behavior change)**: this changes no prompt, tool, skill or model, so it does not pass through the eval gate beyond the existing corpus. It is an interface change with tests, a spec and CI.

## Order and dependencies

1. The spec first, written from the handlers and checked against the Bruno collection, because every later step reads it.
2. The kit (`Authenticated`, `ParamError`), then one feature (`runs`, the largest, with the SSE route) to prove the shape, then the other three.
3. The contract test and the CI check last, because they need all four packages generated to say anything true.

## Risks

- **A generator that wins by default can change a wire shape.** The generated structs replace hand-written ones, so field names, `omitempty` and pointer-ness had to be pinned. Optional strings that the handlers treat as "empty means absent" are `x-go-type-skip-optional-pointer`; a `json.RawMessage` field is `x-go-type`; `budget_usd` is `x-go-name: BudgetUSD` because `USD` is not an initialism the normalizer knows. The handler tests decode the real responses, and the three deliberate differences are in the spec's Assumptions.
- **The generated `ServerInterface` does not enforce a response's shape.** A handler can still answer a status the spec does not list. The strict generator would, but it hands the handler the `*gin.Context` as its `context.Context` (FR-007), so this change keeps the plain one. The spec's response codes are checked by the handler tests, not the compiler.
- **A tool in `go.mod` adds modules.** The change adds fourteen lines to `go.mod` and removes none: the tool line, ten indirect requirements (kin-openapi's and oapi-codegen's own tree), and two direct ones, the runtime the generated code imports and kin-openapi, which the contract test loads and validates the spec with. No existing requirement moves. `govulncheck ./...` does not scan the tool, because the tool's packages are not under `./...`.
- **Generated code is committed.** The alternative, generating at build time, would make `go build ./...` need the tool and a network. The cost is the diff on every spec edit, and the CI check that makes it safe.
- **A rule that cannot fail is worse than none.** Each check in FR-004 was run against an injected violation.
