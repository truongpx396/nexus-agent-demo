# Feature Specification: Delivery and quality gates

**Feature Branch**: `004-delivery-and-quality-gates`

**Created**: 2026-11-02

**Status**: Draft (proposed). Nothing here is implemented. Open decisions are marked `[NEEDS CLARIFICATION]`.

**Input**: The constitution's *Delivery, Scale & Technology Constraints*, *Development Workflow & Quality Gates* and two principles (IV, VII) that the code does not yet meet. `/speckit-converge` found them against `8e02491` (backlog tasks T239–T243 in [spec 001](../001-agent-platform/tasks.md)); each "Current state" was re-checked on 2026-11-02. Requirement ids `FR-401` and up belong to this spec.

Where [spec 003](../003-trust-surface-conformance/spec.md) is about what the running system may do, this one is about how it is built, released and operated.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Durability is defined and rehearsed (Priority: P1)

**Current state**: there is no `docs/durability.md`, and `docs/go-live.md` has no backup or restore item. Nothing defines a recovery point or time objective for the event log or the audit chain, and no restore has been rehearsed.

**Why this priority**: the constitution says "Backup, restore, RPO, and RTO MUST be defined for the event log and audit chain, and restore MUST be rehearsed — an availability SLA without a rehearsed restore is a claim, not a commitment."

**Independent Test**: restore a backup into a clean database and run `nexusd verify-chain` and a projection replay against it.

**Acceptance Scenarios**:

1. **Given** a backup of the event log, the audit chain and the keys, **When** it is restored into an empty database, **Then** `nexusd verify-chain` passes and every session's stored projection equals its replayed one.
2. **Given** the go-live checklist, **When** it is run, **Then** it reports whether a restore has been rehearsed within a stated period, as a manual item printed as a reminder, never a silent pass.

[NEEDS CLARIFICATION: the RPO and RTO numbers, and the backup mechanism (logical dumps, point-in-time recovery, or volume snapshots). **Keys are part of the backup**: the KEK file and the audit signer's key must be restorable, or sealed content is unreadable and the chain cannot be verified, so their custody needs deciding with the mechanism.]

### User Story 2 - Service levels have error budgets and burn-rate alerts (Priority: P2)

**Current state**: `deploy/observability/prometheus-rules.yml` has five alert rules over the golden-signal gauges, whose thresholds spec 001 records as uncalibrated. There are no SLOs, no error-budget policy, no burn-rate alerts and no `docs/runbooks/`.

**Why this priority**: the constitution says every stated availability or latency SLA "MUST have a corresponding SLO, an error-budget policy, and burn-rate alerting that names the runbook it pages to". No SLA is stated today, so the obligation is vacuous until one is; the question is whether to state some.

**Independent Test**: with a synthetic burn, a burn-rate alert fires and its annotation links a runbook that exists.

**Acceptance Scenarios**:

1. **Given** a defined SLO, **When** the error budget burns faster than its window allows, **Then** a fast-burn and a slow-burn alert fire and each names a runbook in `docs/runbooks/`.

[NEEDS CLARIFICATION: which service levels to commit to (run success rate, time to first token, approval latency?), the targets and windows, and who is paged. If none is wanted, the alternative is to state no SLA anywhere, which also satisfies the clause.]

### User Story 3 - The build ships an SBOM and signed artifacts (Priority: P2)

**Current state**: CI runs `govulncheck` on every pull request and weekly, and Dependabot is configured, but no job produces an SBOM or signs anything, and the container images are built in CI but not published.

**Why this priority**: the constitution says "The platform's own build MUST ship an SBOM with signed artifacts and scanned dependencies."

**Independent Test**: build a release; the SBOM lists its dependencies and a signature on each image verifies.

**Acceptance Scenarios**:

1. **Given** a release build, **When** it completes, **Then** an SBOM is attached to it and each published image's signature verifies against a documented identity.

[NEEDS CLARIFICATION: what a "release" is here (a tag, a published image on a registry, neither yet), the SBOM format (SPDX or CycloneDX) and the signing method (keyless signing through the CI provider's OIDC identity is the lowest-maintenance option). Without a publish step there is nothing to sign.]

### User Story 4 - The release gate reports quality-per-dollar and completions-per-million-tokens (Priority: P3)

**Current state**: `evals/efficiency.go` gates token, turn and tool-call bands, but no quality-per-dollar or completions-per-million-tokens figure exists in `evals/`.

**Why this priority**: Principle IV says both "MUST appear in every release gate alongside quality".

**Independent Test**: `make eval` prints both figures for each suite.

**Acceptance Scenarios**:

1. **Given** an eval run, **When** the gate reports, **Then** each suite shows quality per dollar and completions per million tokens next to its pass rate.

[NEEDS CLARIFICATION: gating or reporting only? The default gate runs scripted fake-provider cases whose cost is synthetic, so the figures are only meaningful on the live-model track (the `liveeval` build tag). Report them there and mark the deterministic track "not applicable" rather than invent a price?]

### User Story 5 - Routing has a capability floor (Priority: P3)

**Current state**: `internal/provider/router.go` routes deterministically by data label and difficulty and persists the decision and reason on the session, but has no capability floor for feature demand.

**Why this priority**: Principle VII says routing is "decided by data label and task difficulty (including a capability floor for feature demand), never by model discretion".

**Independent Test**: a table-driven test in which a run that needs a feature the cheapest model lacks is routed to the cheapest model that has it.

**Acceptance Scenarios**:

1. **Given** a run that needs a feature (say, tool calling or a long context), **When** the cheapest eligible model lacks it, **Then** the router picks the cheapest model that has it, and the reason records the floor that applied.

[NEEDS CLARIFICATION: which features count (tool calling, a context-window size, vision, extended reasoning) and where a model's capabilities are declared (the price book, or a model registry beside it).]

## Requirements *(mandatory)*

- **FR-401**: Backup, restore, RPO and RTO MUST be defined for the event log and audit chain, and a restore MUST be rehearsed and recorded (US1).
- **FR-402**: Each stated service level MUST have an SLO, an error-budget policy and burn-rate alerts that name a runbook (US2).
- **FR-403**: A release build MUST produce an SBOM and signed artifacts (US3).
- **FR-404**: Every release gate MUST report quality-per-dollar and completions-per-million-tokens alongside quality (US4).
- **FR-405**: The router MUST apply a capability floor for feature demand, and record it in the routing reason (US5).
