# Implementation Plan: Modular-monolith boundaries

**Spec**: [spec.md](spec.md) | **Status**: Implemented; two follow-ups open

## Summary

Make the three architectural claims (modular monolith, packaged by feature, hexagonal) true beyond persistence, and add the rules that keep them true. Four groups of change, each a self-contained commit that builds and passes on its own: move infrastructure out of the core (US1), move each feature's SQL and each use case out of `store` and `main` into its owner (US2, US3), invert the two cross-feature adapter edges and give the consumers their own interfaces (US4), then declare the shared kernel and the exceptions in the docs. A second pass of five more commits (US5 to US7) closes what the first declared and left: data ownership and its test, the REST and tool bindings beside their features, the connector and skill leaks, the event-log hooks and taxonomy ownership, and the web and test layouts.

## Technical Context

- **Core**: `kernel/`, `internal/{obs,tools,hooks,eventlog,provider,cost,...}`; the adapters it must not reach are `internal/obs/{otlp,langfuse,profiling}`, `internal/hooks/{exechook,httphook}`, `internal/tools/kerneladapter`.
- **Ownership**: `internal/runctl/postgres/{claims,checkpoint,orphans}.go`, `internal/surfaces/postgres/{delivery_sql,inbound_sql,channels}.go`, `internal/tools/postgres/derived_artifacts.go`, `internal/tenancy/`, `internal/audit/postgres/scoped.go`, `internal/contentaccess/`, `internal/reporting/`.
- **Wiring**: `cmd/nexusd/{background,serve,ports,pipeline,controlplane}.go`; the rule on it is `tests/contract/composition_test.go`.
- **Bindings**: `internal/surfaces/rest` (the kit), `internal/{runs,oversight,runctl,contentaccess}/rest`, `internal/{teams,delegate,retrieval}/tool`.
- **Enforcement**: `tests/contract/boundaries_test.go` (core reaches nothing; bindings reach no storage or kernel), `tests/contract/modules_test.go` (adapter isolation, feature dependencies), `tests/contract/composition_test.go` (no SQL in `cmd/nexusd`), `tests/contract/ownership_test.go` (every table has one owner).

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
4. **US5**: data ownership, once the first pass has put most SQL in its owner, so the ownership test starts clean and each exception it finds is a real one (the single genuine cross-feature write was erasure).
5. **US6**: the bindings, after US5, because the REST and tool translation structs in `cmd/nexusd` held logic that belongs in features, and that logic is easier to move once the features' own ports are in place.
6. **US7** last: the leaks and layouts that depend on nothing else, with the web and test renames as pure moves in their own commit.

## Risks

- **A rule that cannot fail is worse than none.** Each rule was run against an injected violation. One check passed vacuously the first time because the mutant was not valid Go, and another because `go test` cached a result computed through a `go list` subprocess; both were redone with `-count=1` and a compilable mutant.
- **The dependency table can rot into a rubber stamp.** It is seeded from the real graph, so it only grows by an edit a reviewer sees; it does not check that a listed edge is still used, on purpose, to keep refactors cheap.
- **Moving SQL changes nothing a unit test sees.** The behavioural check for the moved queries is the integration suite (`go test -race -tags=integration ./...`), run after each group of changes in a clean worktree.
- **The shared kernel was a decision, and then it was a fix.** The first two passes bounded the `sessions` coupling (the ownership test lists each accessor) and left it. The third removed it: stamped columns moved into the feature tables that own them, and the foreign keys went (T037). The migrations are not expand/contract, on purpose, and say so.
- **Not every stamp moves.** `autonomy_level` and `conversational` are `kernel.RunConfig` fields that oversight, plan, delegate and runctl read when they resume a run, and every creator writes. A feature-owned table would make each of them depend on `runctl`; the aggregate is where a value the whole run machinery shares belongs. The test that pins the column list names them with that reason, so adding one is a decision.
- **Absence means root.** A session nobody spawned has no `session_lineage` row, so creating a thread's first session needs no knowledge of delegation. The cost is that readers treat a missing row as "root at depth 0" (`COALESCE` in the per-root count), which is why `ParentContext` still loads the session first: a session that does not exist must fail closed, not read as a root.
- **Three copies of a literal agreed by luck.** The digest defaults were `"phase2-v1"` in runs, cron and fork, and a fork compares its digest with its parent's. They are one named constant now.
- **A moved translator can lose a behaviour.** The two `cmd/nexusd` structs held logic, not just mapping. Each piece became a feature use case with unit tests (`oversight.Decisions`, `runctl.Control.Send`) before the handler was pointed at it, and the handlers gained tests they never had.
- **Pure renames hide a broken reference.** The web and test moves were scripted from a rename list, and `tests/contract/specs_test.go` fails on a task naming a path that no longer exists, which caught every stale spec reference; `tsc`, `oxlint` and the build caught the TypeScript.
