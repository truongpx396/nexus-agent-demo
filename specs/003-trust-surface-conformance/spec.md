# Feature Specification: Trust-surface conformance

**Feature Branch**: `003-trust-surface-conformance`

**Created**: 2026-11-02

**Status**: Draft (proposed). Nothing here is implemented. Open decisions are marked `[NEEDS CLARIFICATION]`.

**Input**: The constitution's *Security & Trust Surface* constraints that the code does not yet meet. `/speckit-converge` found them against `8e02491` (backlog tasks T232–T238 in [spec 001](../001-agent-platform/tasks.md)); each "Current state" below was re-checked against the code on 2026-11-02.

Each story is a `MUST` in [the constitution](../../.specify/memory/constitution.md) that has no shipped evidence today. They share a spec because they share a source and an owner, not because they ship together. Requirement ids `FR-301` and up belong to this spec.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A webhook cannot be replayed, and one sender cannot exhaust a tenant (Priority: P1)

**Current state**: Telegram (a secret-token header), Zalo (an HMAC over the body) and email (Basic Auth) are each verified before the payload is parsed, and a provider's redelivery is caught by its delivery id. But no handler checks a timestamp or nonce window, and the three rate limiters are keyed by **tenant**: each handler calls `RateLimit.Allow(tenantID.String())` (`internal/surfaces/{telegram,zalo,email}/webhook.go`), so one sender can use up a tenant's whole channel. The limiters are in-memory token buckets, so the limit is per process.

**Why this priority**: the constitution (*Inbound channel authenticity*) requires a surface that accepts unsolicited traffic to "reject replays outside a bounded window, and rate-limit per external identity **before** the kernel sees the payload".

**Independent Test**: replay a captured, validly signed delivery after the window, and send from one sender faster than its limit while another sender in the same tenant sends normally.

**Acceptance Scenarios**:

1. **Given** a correctly authenticated delivery older than the window, **When** it is posted, **Then** it is rejected before any run starts, and the rejection is counted.
2. **Given** one external sender exceeding its limit, **When** a different sender in the same tenant posts, **Then** the second is served.
3. **Given** more than one `nexusd` process, **When** a sender spreads requests across them, **Then** [NEEDS CLARIFICATION: must the limit hold across processes (a shared store), or is a per-process bucket acceptable until a multi-process deployment exists?].

[NEEDS CLARIFICATION: the window length, and which replay signal each provider actually offers (a signed timestamp, a nonce, or a monotonic id); this differs per provider and has to be checked against each one's documentation.]

### User Story 2 - Telemetry has no content channel (Priority: P1)

**Current state**: `NEXUS_TRACE_CONTENT=true` (`cmd/nexusd/serve.go`) turns on `kernel.TraceContent`, which calls `Span.SetContent`; the OTLP and Langfuse exporters write that content **bypassing the attribute allowlist** (`internal/obs/otlp/exporter.go`). `TestKernelTracerContentOptIn` (`tests/integration/tracer_test.go`) asserts that behaviour. The docs call it a "sanctioned exception".

**Why this priority**: the constitution says telemetry is "a content-free signal class enforced by a deny-by-default attribute allowlist, and **no flag may admit content to it**", and that plaintext is inspectable only through an audited, expiring content-access grant that emits a receipt on grant and on every read. This one is a violation, not a gap, so it changes what is allowed.

**Independent Test**: with every flag set, no exported span attribute contains prompt or tool content; reading content needs a grant that is receipted.

**Acceptance Scenarios**:

1. **Given** the resolution chosen below, **When** the system runs with any configuration, **Then** no span carries content except through a path that emits a receipt.
2. **Given** `TestKernelTracerContentOptIn`, **When** the resolution lands, **Then** the test asserts the new behaviour, not the violation.

[NEEDS CLARIFICATION: which resolution? (a) remove `NEXUS_TRACE_CONTENT` and `Span.SetContent`, so debugging reads go through the content-access grant; or (b) amend the constitution (a MINOR version) so a dev-only content channel exists, is refused outside `--dev`, and emits a receipt. (a) is the smaller change and matches the text as written.]

### User Story 3 - The audit chain's head is anchored outside the database, and a break raises an alert (Priority: P1)

**Current state**: anchors are signed rows in `audit_anchors` in the same Postgres that holds the chain (`internal/audit/postgres/anchor.go`), so deleting a chain's tail together with its anchors is undetectable. When the scheduled verifier finds a break or a gap it logs an error (`cmd/nexusd/background.go`); nothing is exposed on `/metrics` and no Prometheus rule fires.

**Why this priority**: the constitution requires "the chain head MUST be periodically anchored outside the writing system" and that the verifier "MUST alert on a break or a gap in sequence".

**Independent Test**: through the owner connection, delete the tail of a chain and its anchors, and separately alter, delete and reorder a receipt; the verifier reports each, and a counter and an alert follow.

**Acceptance Scenarios**:

1. **Given** the tail and its anchors deleted from the database, **When** the verifier runs, **Then** it reports a break by comparing against the external anchor.
2. **Given** any break or gap, **When** the verifier finds it, **Then** a counter on `/metrics` is non-zero and a Prometheus rule fires.

[NEEDS CLARIFICATION: the external sink. An object store with object-lock, an append-only log service, or a second database all work; the choice depends on where this is deployed, and it should be behind a port like the signer is.]

### User Story 4 - The harness digest pins the behavior a run used (Priority: P2)

**Current state**: `harness.Digest` hashes the system-prompt version, catalog manifest, skill set, safety-policy version, approval-policy version and prompt mode (`internal/harness/digest.go`), but its three production call sites (`internal/runs/launch.go`, `internal/runctl/fork.go`, `internal/surfaces/cron/scheduler.go`) never set `SafetyPolicyVersion` or `ApprovalPolicyVersion`, and hard-code `SystemPromptVersion: "phase2-v1"`. So the digest does not move when a policy or the prompt changes, which makes a fork's "digest divergence" report and any reproducibility claim weaker than they read.

**Why this priority**: the constitution says every run "MUST persist the digest of the configuration that determined its behavior, or it is not reproducible".

**Independent Test**: change the safety policy, the approval policy and the system prompt in turn; the digest of a new run changes each time, and does not change when nothing changed.

**Acceptance Scenarios**:

1. **Given** a change to the safety policy, approval policy or system prompt, **When** a run starts, **Then** its digest differs from the previous run's.
2. **Given** no change, **When** two runs start, **Then** their digests are equal.

[NEEDS CLARIFICATION: the version identifier. Deriving it from a hash of the policy and prompt content cannot be forgotten; a hand-maintained version string can. The first is the safer default.]

### User Story 5 - Content is masked by class before it leaves the trust boundary (Priority: P2)

**Current state**: nothing masks PII, secrets, PHI or card data. Content leaves the boundary through provider calls (`internal/provider/`) and outbound deliveries (`internal/surfaces/outbox.go` and the per-surface senders). It is bounded (an egress allowlist for `web_fetch`, a no-network sandbox) but not masked.

**Why this priority**: the constitution says outbound domains MUST be allowlisted and "PII/secrets/PHI/card data MUST be masked by class before leaving the trust boundary", and that sensitive tasks SHOULD route to a self-hosted model.

**Independent Test**: a prompt containing a card number and an API key reaches a provider (a recording fake) with both masked, and the sealed event log still holds the original.

**Acceptance Scenarios**:

1. **Given** content containing a masked class, **When** it is sent to a provider or an outbound surface, **Then** the recipient receives the masked form.
2. **Given** a self-hosted in-VPC provider, **When** a run is routed to it, **Then** [NEEDS CLARIFICATION: is masking skipped, since the content never leaves the boundary?].

This story is larger than the other four and needs its own full `/speckit-specify` pass before it is built. [NEEDS CLARIFICATION: the classes and how each is detected (pattern, checksum, model); whether masking is reversible; the false-positive policy; and whether tool inputs are masked as well as prompts.]

## Requirements *(mandatory)*

- **FR-301**: A webhook handler MUST reject a delivery outside a bounded replay window before the kernel sees it (US1).
- **FR-302**: A webhook rate limit MUST be keyed by the external sender, not the tenant (US1).
- **FR-303**: No configuration may place prompt or tool content on a telemetry span except through a path that emits a receipt (US2).
- **FR-304**: The audit chain's head MUST be anchored in a system the writing database cannot rewrite, and the verifier MUST cross-check it (US3).
- **FR-305**: A chain break or gap MUST increment a metric and fire an alert that names a runbook (US3).
- **FR-306**: A run's harness digest MUST change when its safety policy, approval policy or system prompt changes (US4).
- **FR-307**: Content of a masked class MUST NOT reach a provider or an outbound delivery unmasked (US5).

## Out of scope

Holding the audit signing key in a KMS or HSM (the constitution asks for it; the key is a file today) and a timestamp-bounded replay window for providers that offer no signal are both noted in [spec 001](../001-agent-platform/spec.md) *Known deviations* and are not decided here.
