# Implementation Plan: Delivery and quality gates

**Spec**: [spec.md](spec.md) | **Status**: Draft (proposed)

## Summary

Five conformance gaps in how the platform is built, released and operated, ordered by risk: a restore that has never been rehearsed first, then service levels, then supply-chain attestation, then the two quality-gate metrics.

## Technical Context

- **Durability**: the stores are Postgres (the event log, the audit chain, the price book, tenant config) and the files `nexusd` and `signerd` hold (`.dev/kek.key`, the signer's key). `nexusd verify-chain` and the projection replay in `internal/eventlog` are the checks a restore must pass. `docs/go-live.md` and `cmd/nexusd/cli_golive.go` are where a restore item goes.
- **Service levels**: `deploy/observability/prometheus-rules.yml` (five rules), `docs/observability.md`, the golden signals in `internal/reporting/golden_signals.go`.
- **Supply chain**: `.github/workflows/ci.yml` (including the `images` job) and `govulncheck.yml`; the images are built by the root `Dockerfile` (static, distroless, non-root).
- **Release gate**: `evals/{efficiency,gate,types}.go`; `make eval` is a CI job on the deterministic corpus, and live-model evals sit behind the `liveeval` build tag.
- **Routing**: `internal/provider/router.go` and `router_test.go`.

## Constitution Check

Each story cites the clause it closes (Delivery: durability is designed, not assumed; Workflow: SLOs have error budgets; Security & Trust: compliance and SBOM; Principles IV and VII). The constitution's own rule that a plan "MUST record the deferral explicitly" applies: any story left unbuilt after this spec stays listed in [spec 001's backlog](../001-agent-platform/tasks.md#convergence-backlog) as deferred, with its trigger.

## Order and dependencies

1. **US1 (durability)** first. It is the one whose absence turns a bad day into a lost one, and its keys question also constrains how backups are taken.
2. **US2 (SLOs)** needs a decision on whether to state any SLA at all before it has work to do.
3. **US3 (SBOM and signing)** needs a publish step to exist; if it does not, building one is the first task.
4. **US4 and US5** are small and independent; US4 depends on the live-eval track to be meaningful.

## Risks

- **A rehearsed restore needs a place to restore to.** The integration suite already starts a disposable Postgres with testcontainers, which is the natural harness for an automated rehearsal; a rehearsal against the real stack is a manual go-live item.
- **Quality-per-dollar on a scripted corpus is a number that means nothing.** Presenting it without that caveat would be the kind of overstatement this project has been correcting elsewhere.
- **A capability floor needs a source of truth for what each model can do**, which does not exist beside the price book yet.
