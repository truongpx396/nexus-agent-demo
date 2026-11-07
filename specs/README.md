# Specs

The specs are the single source of truth for what this project does, what has been built and what is open. They replace the build ledger
(`docs/build-phases.md`), which is deleted: its last version is `git show 09d54f8:docs/build-phases.md`.

Each directory is one feature (or, like 001 and 002, a set of related stories), with the three Spec Kit files: `spec.md` (what and why), `plan.md` (how, and the constitution check) and
`tasks.md` (the work, one line per task). `.specify/memory/constitution.md` is the review checklist for all of them.

## Index

| Spec | Feature | Status | Build-ledger phases |
|---|---|---|---|
| [001](001-agent-platform/spec.md) | The Nexus Agent platform (baseline) | Implemented; backlog open ([tasks](001-agent-platform/tasks.md#convergence-backlog)) | 0–17, plus the work shipped after the ledger |
| [002](002-post-baseline-increments/spec.md) | Post-baseline increments: webhook idempotency, hexagonal architecture, a provable topology seam | Implemented (retroactive) | 18, 19, 20 |
| [003](003-trust-surface-conformance/spec.md) | Trust-surface conformance: webhook replay and per-sender limits, the telemetry content channel, audit anchoring and alerting, digest pinning, egress masking | Draft (proposed); open decisions marked | none (new work) |
| [004](004-delivery-and-quality-gates/spec.md) | Delivery and quality gates: backup and restore, SLOs, SBOM and signing, release-gate metrics, a routing capability floor | Draft (proposed); open decisions marked | none (new work) |
| [005](005-modular-monolith-boundaries/spec.md) | Modular-monolith boundaries: infrastructure out of the core, each table owned by its feature, a composition root that only wires, and the rules that enforce it | Implemented; follow-ups open ([tasks](005-modular-monolith-boundaries/tasks.md#open-follow-ups)) | none (new work) |

Also in [001](001-agent-platform/): [pattern-coverage.md](001-agent-platform/pattern-coverage.md), the original design's 67 patterns against this demo.

## Phase to spec

Code comments cite `Phase N` and `task N.M` from the build ledger. Those ids are kept: a ledger task is a task in a spec's `tasks.md`, marked
`(bp N.M)`. This table says which spec a phase is in; `tests/contract/specs_test.go` fails if a citation in the code resolves to neither.

| Phase | Spec | Where in the spec |
|---|---|---|
| 0 | 001 | Setup |
| 1 | 001 | Foundational |
| 2 | 001 | US1: run an agent task |
| 3 | 001 | US2: governed tool use |
| 4 | 001 | US3: costs that cannot run away |
| 5 | 001 | US4: a trust surface |
| 6 | 001 | US5: a run that survives failure |
| 7 | 001 | US6: memory, skills, surfaces |
| 8 | 001 | US7: plans and delegation |
| 9 | 001 | US10: peer teams |
| 10 | 001 | US8: eval gate and go-live |
| 11 | 001 | US11: more surfaces |
| 12 | 001 | US12: retrieval |
| 13 | 001 | US9: production readiness |
| 14 | 001 | US9: production readiness |
| 15 | 001 | US9: production readiness |
| 16 | 001 | US13: real capability behind the seams |
| 17 | 001 | US14: see what the platform is doing |
| 18 | 002 | US1: inbound-delivery dedup, session locking, atomic resume |
| 19 | 002 | US2: hexagonal architecture |
| 20 | 002 | US3: a provable topology seam |

## Legacy task-number aliases

Code comments cite `task 13.8` to `13.15`; the ledger later split its fifteen-task Phase 13 into Phases 13, 14 and 15. A citation of the left-hand id
resolves to the right-hand `(bp)` id.

| Code comment says | Resolves to | Closes |
|---|---|---|
| 13.8 | 14.1 | F9: unit tests for the untested packages |
| 13.9 | 14.2 | F6: live-model eval track |
| 13.10 | 14.3 | F11: `govulncheck`, Dependabot |
| 13.11 | 14.4 | F12: validated config, no silent KEK |
| 13.12 | 15.1 | F13: OTLP exporter, `/metrics` |
| 13.13 | 15.2 | F10: sandbox hardening |
| 13.14 | 15.3 | F14: pool sizing, SSE buffer, load test |
| 13.15 | 15.4 | F15: `internal/controlplane` |

## Where the build ledger went

| It held | Now |
|---|---|
| The task rows, Phases 0–17 | [001's tasks.md](001-agent-platform/tasks.md), one line each, ids kept as `(bp N.M)` |
| The task rows and narrative, Phases 18–20 | [Spec 002](002-post-baseline-increments/spec.md), carried over verbatim |
| The 67-pattern coverage map | [001/pattern-coverage.md](001-agent-platform/pattern-coverage.md) |
| The risk register | the end of [001/plan.md](001-agent-platform/plan.md) |
| The phase narratives and long rows, Phases 0–17 | `git show 09d54f8:docs/build-phases.md` only; 001 keeps the one-line tasks |
| The effort table | dropped: it was never updated past Phase 13 |

## How work flows

- **A new feature** starts with `/speckit-specify`, then `/speckit-plan` and `/speckit-tasks`, which create the next `specs/NNN-name/`. Open questions stay marked `[NEEDS CLARIFICATION]` until someone with the authority to decide has; do not guess a number into a spec.
- **A change to something already specified** adds or checks off a task in that spec's `tasks.md` and names the files it touched. Related small increments share a spec as stories, as 001 and 002 do, instead of each getting a directory.
- **A gap found by audit** (`/speckit-converge`, `/speckit-analyze`, a review) becomes a task or a story in the spec it belongs to, not a note elsewhere. Run `/speckit-converge` before a release.
- **The gate**: `scripts/spec-gate.sh` (`make spec-gate`, and the `spec-gate` workflow in CI) fails a pull request that changes behaviour-bearing code (non-test Go under `internal/`, `cmd/`, `kernel/` and `evals/`, SQL migrations, the eval corpus, `web/src/`) without changing anything under `specs/`. A change that genuinely needs no spec carries the `no-spec` label and says why in the description.
- **Ids**: a task that closes a ledger-era item keeps its `(bp N.M)`. New work has none and is cited by spec and task, for example `spec 003 T004`.

## What is checked

`tests/contract/specs_test.go` runs with the unit tests and fails on:

- a spec directory missing `spec.md`, `plan.md` or `tasks.md`;
- a `task N.M` citation anywhere in the code or docs that is neither a `(bp N.M)` id in some `tasks.md` nor a legacy alias above;
- a `Phase N` citation in Go code that is not in the table above;
- a `(bp N.M)` id that appears in more than one spec;
- a path named by a `[X]` task, in a spec whose `tasks.md` declares its paths verified, that does not exist at `HEAD`;
- a relative Markdown link inside `specs/` that points at a file that does not exist;
- the two copies of the constitution differing.

The gate above is separate: it checks that a *change* touches a spec; the tests check that the specs' ids, paths and links still hold.

None of that can tell whether a sentence is *true*. That is what `/speckit-analyze` and `/speckit-converge` are for.
