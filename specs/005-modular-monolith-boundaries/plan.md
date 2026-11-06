# Implementation Plan: Modular-monolith boundaries

**Spec**: [spec.md](spec.md) | **Status**: Implemented; follow-ups open

## Summary

Make the three architectural claims (modular monolith, packaged by feature, hexagonal) true beyond persistence, and add the rules that keep them true. Four groups of change, each a self-contained commit that builds and passes on its own: move infrastructure out of the core (US1), move each feature's SQL and each use case out of `store` and `main` into its owner (US2, US3), invert the two cross-feature adapter edges and give the consumers their own interfaces (US4), then declare the shared kernel and the exceptions in the docs.

## Technical Context

- **Core**: `kernel/`, `internal/{obs,tools,hooks,eventlog,provider,cost,...}`; the adapters it must not reach are `internal/obs/{otlp,langfuse,profiling}`, `internal/hooks/{exechook,httphook}`, `internal/tools/kerneladapter`.
- **Ownership**: `internal/runctl/postgres/{claims,checkpoint,orphans}.go`, `internal/surfaces/postgres/{delivery_sql,inbound_sql,channels}.go`, `internal/tools/postgres/derived_artifacts.go`, `internal/tenancy/`, `internal/audit/postgres/scoped.go`, `internal/contentaccess/`, `internal/reporting/`.
- **Wiring**: `cmd/nexusd/{background,serve,ports,pipeline,controlplane}.go`; the rule on it is `tests/contract/composition_test.go`.
- **Enforcement**: `tests/contract/boundaries_test.go` (core reaches nothing), `tests/contract/modules_test.go` (adapter isolation, feature dependencies), `tests/contract/composition_test.go` (no SQL in the serving path).

## Constitution Check

- **Principle I (one loop, many surfaces)**: strengthened. The loop's package no longer links any surface, control plane, tool pipeline or driver, and a contract test says so.
- **Principle II (immutable models, append-only state)**: unchanged. The event log, its trigger, its sealing and the receipt-in-the-same-transaction guarantee are not touched; the writers that share a transaction are injected, not removed.
- **Principle VI (tenant first; audit and observability day-one)**: unchanged. Every moved query keeps its `InTenantTx` scope; the one cross-tenant read (`tenancy.Directory`) is isolated behind a port and connects as the operator, as the code it replaces did.
- **Principle VII (model- and provider-agnostic by abstraction)**: unchanged; the provider port and its adapters are not moved.
- **Principle IX (govern every behavior change)**: this change alters no prompt, tool, skill or model, so it does not pass through the eval gate beyond the existing corpus, which is unchanged. It is a structural change with tests, a spec and CI.

## Order and dependencies

1. **US1** first: it is the claim most visibly false, and the others rest on a core that is allowed to be pure.
2. **US2 and US3** together: `store`'s feature SQL and `main`'s use cases are the same code seen from two sides (the checkpoint is both), so moving one without the other leaves a broken build.
3. **US4** last: it needs the adapters to be where they belong before an isolation rule can be written that is true.

## Risks

- **A rule that cannot fail is worse than none.** Each rule was run against an injected violation. One check passed vacuously the first time because the mutant was not valid Go, and another because `go test` cached a result computed through a `go list` subprocess; both were redone with `-count=1` and a compilable mutant.
- **The dependency table can rot into a rubber stamp.** It is seeded from the real graph, so it only grows by an edit a reviewer sees; it does not check that a listed edge is still used, on purpose, to keep refactors cheap.
- **Moving SQL changes nothing a unit test sees.** The behavioural check for the moved queries is the integration suite (`go test -race -tags=integration ./...`), run after each group of changes in a clean worktree.
- **The shared kernel is a decision, not a fix.** `sessions` is referenced by every feature table; calling that out in the README is honest, and it means "extract one feature into its own service" still needs schema work.
