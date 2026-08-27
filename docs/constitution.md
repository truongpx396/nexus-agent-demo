<!--
SYNC IMPACT REPORT
==================
Version change: 1.1.0 → 1.2.0
Bump rationale: MINOR — materially expanded the Development Workflow
  observability rule and added a state-artifact rule, closing gaps found in the
  2026-07-30 observability & state-management design review. No principle removed
  or redefined; all 1.1.0 rules remain binding.

Modified sections (1.1.0 → 1.2.0):
  - Development Workflow & Quality Gates → "Observability captures structure, not
    content" now names the mechanism (deny-by-default attribute allowlist, no
    content-admitting flag) and reconciles telemetry with the crypto-shredding
    erasure boundary; debugging access to prompts/responses is now an audited,
    scoped, expiring grant emitting a receipt per read.
  - Development Workflow & Quality Gates → new "State artifacts are not
    interchangeable": compaction / checkpoint / snapshot separated; write-ahead
    effect claims resolved by probe or human, never re-execution; replay vs
    resume vs fork named; per-run configuration digest required for
    reproducibility.

Templates requiring updates: none (generic Constitution Check remains compatible)
Follow-up TODOs: none

---
PRIOR REPORT (1.0.0 → 1.1.0)
Bump rationale: MINOR — materially expanded existing principles and constraint
  sections to close design-review gaps. No principle removed or redefined; all
  1.0.0 rules remain binding.

Modified principles:
  - VI (Tenant Is the First Dimension) — isolation must survive connection
    pooling (transaction-local tenant scoping); the isolation test must run
    through the production pooling tier.
  - IX (Verify / Govern) — evals are a Foundational deliverable, not a later
    phase; a deterministic test harness is required for a non-deterministic
    dependency.

Added / expanded sections:
  - Security & Trust Surface: audit receipts must be hash-chained and externally
    anchored with sign-only key custody; customer content encrypted at rest with
    per-tenant keys; erasure via crypto-shredding reconciled with the append-only
    log; metadata egress from a customer boundary must be explicit and bounded.
  - Delivery, Scale & Technology Constraints: cost ceilings enforced by
    reserve-then-reconcile; event schema is versioned with an upcasting path;
    expand/contract schema migration; documented RPO/RTO and rehearsed restore.
  - Development Workflow & Quality Gates: SLO/error-budget policy; operating
    model ownership (platform team / AgentOps / governance sign-off).

Removed sections: none

Templates requiring updates:
  - .specify/templates/plan-template.md ✅ compatible (generic Constitution Check)
  - .specify/templates/spec-template.md ✅ compatible
  - .specify/templates/tasks-template.md ✅ compatible
  - .github/agents/speckit.*.agent.md ✅ no outdated references found

Follow-up TODOs: none
-->

# Nexus Agent Constitution

Nexus Agent is a single, model-agnostic, production-grade AI agent platform: one
reliable kernel loop wrapped in a carefully engineered harness, exposed through
thin surface adapters, fronted by a control plane, and grounded in a trust
surface. This constitution encodes the non-negotiable rules that govern how it is
designed, built, and shipped. When a design conflicts with a principle here, the
design loses.

## Core Principles

### I. One Loop, Many Surfaces

A single agent kernel MUST power every surface (CLI, chat, API, cron, email, IDE).
Surfaces are thin translators of input/output and MUST NOT fork or re-implement
agent control flow. The loop is the only place control flow lives and MUST be a
single async generator with typed, exhaustively-handled terminal states.

**Rationale**: Forked logic per surface multiplies bugs and defeats replay, audit,
and consistent behavior. Concentrating control flow in one small kernel keeps it
debuggable and lets every surface inherit the same guarantees.

### II. Immutable Models, Append-Only State

Agent, Tool, LLM, and configuration objects MUST be immutable. The only mutable
runtime state is the conversation state, which MUST change by appending typed
events to an append-only event log — never by in-place mutation. Every
`tool_use` MUST have a paired `tool_result` (a synthetic result on any
cancel/error path) before the next model call.

**Rationale**: An append-only event log is the single source of truth that buys
replay, deterministic debugging, tamper-evident audit, and safe parallelism. The
paired-result invariant is the #1 correctness rule; violating it corrupts
transcripts and produces production failures.

### III. Cache-Stable Context Is Architecture

The prompt MUST be structured as a byte-stable prefix (stable system prompt, tool
schema catalog, append-only transcript) followed by a volatile tail rebuilt each
turn. Anything per-turn is structurally banned from the prefix; the system prompt
MUST NOT be mutated mid-session. Context management MUST target >90% cache-read on
steady-state turns; compaction MUST be structured and run off the paying loop.

**Rationale**: Input tokens are ~90% of the bill and cache-read is the single
highest-leverage cost and throughput lever. Cache stability is architecture, not a
late optimization — every design either preserves cache hits or is rejected.

### IV. Stop on Cost, Not Vibes

Cost MUST be the primary stop signal for every run; iteration count and wall-clock
timeout are backstops only. Every task chain MUST meter input and output tokens
per turn and enforce hard per-task and per-tenant cost ceilings that terminate
with an explicit `cost_exhausted` reason. Quality-per-dollar and completions-per-
million-tokens MUST appear in every release gate alongside quality.

**Rationale**: Step counts vary ~5× across models, so counting steps is a false
signal; token usage explains most task-performance variance. Metering in the same
layer that spends tokens makes cost observable, boundable, and attributable.

### V. Safety Is Per-Invocation and Fails Closed

Safety MUST be evaluated per invocation on parsed input, not per tool
(`Bash("ls")` ≠ `Bash("rm -rf")`). Tools MUST default to fail-closed (serial
unless proven concurrency-safe, assume writes, permission denied unless explicitly
granted). Defense MUST be layered (channel, autonomy, workspace, shell, sandbox,
audit), and all tool output and retrieved content MUST be treated as untrusted and
never fed straight into execution. Within a session, at most two of {process
untrusted input, access private data, change state or communicate externally} are
permitted without human approval (the Rule of Two).

**Rationale**: Prompt injection is unsolved; a "95% blocked" guardrail is a failing
grade. Safety must be designed in by breaking the lethal trifecta and failing
closed, so a forgotten flag yields slow behavior, never data loss or a breach.

### VI. Tenant Is the First Dimension; Audit and Observability Are Day-One

Tenant identity MUST be the first dimension of every session key, database row,
workspace path, cost record, and secret. Data isolation MUST be enforced at the
database (row-level security), never by application ACLs alone. Every action MUST
be attributable to a user and tenant in an immutable audit log, and per-turn
token/cost/latency observability MUST exist from the first pilot. Secrets, budgets,
and rate limits MUST be isolated per tenant.

Isolation MUST survive connection pooling. Tenant scoping MUST be
**transaction-local** (`SET LOCAL` / `SET ROLE LOCAL` inside the transaction);
session-level scoping is prohibited because a transaction-pooling tier reassigns
a connection between tenants. The cross-tenant isolation test MUST execute
through the same pooling tier production uses — a test that passes only against a
direct connection proves nothing.

**Rationale**: The enterprise tax — multi-tenancy, audit, observability — is a
day-one requirement; retrofitting it is a rewrite. DB-level isolation is the only
defense that survives an application bug — and only if the mechanism that carries
tenant identity survives the connection pooler in front of it.

### VII. Model- and Provider-Agnostic by Abstraction

All provider access MUST go through one internal abstraction with a single
normalized stream contract; scattered SDK calls are prohibited. Native tool-calling
only — no parsing tools out of free-form text. Model routing MUST be deterministic
and auditable, decided by data label and task difficulty (including a capability
floor for feature demand), never by model discretion; regulated payloads MUST be
routable to a self-hosted in-VPC model.

**Rationale**: The model is roughly fixed for the life of the project and the
harness is the durable asset. Standardizing the harness (not the model) enables
multi-provider failover, capacity spreading, and no-egress compliance without code
forks.

### VIII. Reliability: Classify, Resume, Never Silently Retry

Every failure MUST be classified into a typed class before any retry; retries MUST
be logged with a reason, backed off with jitter, and circuit-broken after an
identical failing call repeats three times. Silent retries are prohibited. State
MUST be checkpointed to durable storage so runs resume from the last checkpoint
rather than restarting. Stuck detection (repeated actions, oscillation, zero net
change) MUST break the loop, and deploys MUST NOT cut a running agent over
mid-task.

**Rationale**: In agentic systems minor issues derail agents entirely and errors
compound; classification, durable resume, and circuit-breaking convert fragile
long runs into recoverable ones and keep failure spend bounded and observable.

### IX. Verify Against Acceptance Criteria; Govern Every Behavior Change

Agents MUST NOT self-declare success; completion MUST be verified against explicit
acceptance criteria (tests pass, build green, schema validates, end-state graded).
Prompts, tools, and skills are production config and MUST be versioned, code-
reviewed, and eval-gated; a prompt or model change is a deploy. Agent-written
skills MUST follow propose → human/eval gate → version → promote, and MUST NEVER be
auto-promoted.

The eval set and its CI gate are a **Foundational** deliverable that MUST exist
before the first behavior-bearing slice ships, not a later phase — the window in
which changes have the largest effect sizes is the window in which they are
otherwise unmeasured. Because the model is a non-deterministic dependency, the
test suite MUST run against a deterministic provider harness (recorded
transcripts / fake provider) so correctness tests neither flake nor bill.

**Rationale**: Non-determinism and spec-gaming make self-reported success
unreliable. Treating prompts/tools/skills as governed, eval-gated config is what
separates a demo from a system that can be safely changed and audited — and an
eval gate that arrives after three phases of unmeasured change has already lost
the baseline it was meant to protect.

## Security & Trust Surface

These constraints make the platform signable by a security-conscious enterprise and
are binding in addition to Principle V:

- **Secrets**: Never placed in the prompt. Injected at tool-execution time from a
  vault; the model sees only a handle. Credentials MUST be isolated per tenant.
- **Identity**: The agent MUST act with the calling user's RBAC scope (act-as
  delegated identity), never a god-mode service account. RBAC is enforced at the
  tool boundary, not just the UI.
- **Audit receipts**: Mutating actions MUST produce tamper-evident tool receipts
  (HMAC over session + tool name + args + result + timestamp). A per-record MAC
  alone is NOT tamper-evidence: receipts MUST be **hash-chained** per session
  (each carrying its predecessor's digest), the chain head MUST be periodically
  anchored outside the writing system, and the signing key MUST be held
  sign-only (KMS/HSM) so a component that writes receipts cannot rewrite them. A
  scheduled verifier MUST prove chain continuity and MUST alert on a break or a
  gap in sequence.
- **Egress & redaction**: Outbound domains MUST be allowlisted; PII/secrets/PHI/
  card data MUST be masked by class before leaving the trust boundary. Sensitive
  tasks SHOULD route to a self-hosted model so payloads never leave.
- **Content at rest**: Conversation content (prompts, tool arguments, tool
  results) is customer data. It MUST be encrypted at rest under a per-tenant key,
  with customer-managed / bring-your-own-key supported at the deployment tiers
  that require it.
- **Erasure reconciled with append-only**: An append-only log and a right to
  erasure are reconciled by **crypto-shredding**, not by deletion — event
  payloads are envelope-encrypted per tenant (and per erasure subject where the
  tier requires it) and erasure destroys the key, rendering content
  unrecoverable while the event sequence, its digests, and the audit chain stay
  intact and verifiable. Redaction MUST NOT break chain verification.
- **Bounded metadata egress**: When a data plane runs inside a customer
  boundary, everything that leaves it MUST be enumerated in the contract and
  bounded to structure (identifiers, counts, digests) — never content. A
  fully in-boundary audit/telemetry sink MUST be selectable by configuration.
- **Human-in-the-loop**: Payments, deletes, external sends, and production changes
  MUST be gated by scoped human approval; the sandbox is the trust boundary. An
  approval that expires denies **the action**, not necessarily the run: the run
  MUST be offered the typed denial and MAY replan; terminating the run is the
  fallback when it cannot proceed. Approval scopes MUST be bounded in time and
  in blast radius — no scope may permanently ungate a class of high-impact action.
- **Inbound channel authenticity**: Any surface that accepts unsolicited inbound
  traffic (webhooks, callbacks) MUST verify provider authenticity
  (signature/secret), reject replays outside a bounded window, and rate-limit per
  external identity **before** the kernel sees the payload. Identity binding is a
  separate control and does not substitute for it.
- **Compliance**: Data residency/region pinning, retention limits, DSAR support, a
  no-train guarantee, model/prompt versioning, and an AI risk register MUST be
  maintained where the deployment tier requires them. The platform's own build
  MUST ship an SBOM with signed artifacts and scanned dependencies.

## Delivery, Scale & Technology Constraints

- **Control-plane / data-plane split**: The control plane (auth, RBAC, routing,
  budgets, eval/skill/MCP catalogs, audit sink) and the data plane (kernel loop,
  sandboxes, memory, provider calls) MUST remain separately deployable behind a
  versioned contract, so "move the data plane into the customer VPC" is a
  deployment flag, not a rewrite. The same build MUST serve multi-tenant SaaS,
  single-tenant, self-hosted/BYOC, and hybrid topologies via configuration.
- **Configuration, not forks**: Per-organization behavior (tenant config, agent
  definition, skills, surfaces, connectors) MUST be data/config read at runtime.
  The kernel MUST NEVER be forked per customer. Connectors MUST attach only through
  the vetted, per-tenant, RBAC-scoped MCP catalog.
- **Runtime is stateless with externalized state**: Agent runs are async jobs on a
  durable queue processed by stateless, disposable workers. Routing MUST be by
  session key (per-session serial, cross-session concurrent). Sandboxes MUST come
  from a warm pool with hard TTLs, reclamation, and per-tenant caps. Provider TPM
  MUST be handled via per-tenant rate limits, connection pooling, failover-as-
  capacity, and cached prefixes.
- **Memory is files first**: Start file-based, inject immutably at session start
  (updates take effect next session), scope per tenant with retention limits, and
  scan for injection/exfiltration before injecting. A vector DB / knowledge graph
  is introduced only when the data shape and scale (past ~1M tokens of durable
  knowledge, or genuinely graph-shaped relational data) justify it.
- **Cost ceilings are enforced before the spend, not after**: A ceiling that is
  checked by aggregating usage after a turn completes is a lagging indicator, not
  a ceiling. Every turn MUST reserve its worst-case cost against an atomic
  per-tenant counter before the model call, the worker MUST enforce a local hard
  per-run budget synchronously, and actuals MUST be reconciled against the
  reservation afterwards.
- **The event log is a versioned contract**: Every appended event MUST carry a
  schema version, and a documented upcasting path MUST keep historical events
  replayable across schema change. Derived tables (session status, approval
  status, sandbox state) are projections of the log, never a second source of
  truth.
- **Schema change is expand/contract**: Because a rolling deploy runs old and new
  code against one database, migrations MUST be additive-then-cleanup, and no
  release may require old and new schema simultaneously.
- **Durability is designed, not assumed**: Backup, restore, RPO, and RTO MUST be
  defined for the event log and audit chain, and restore MUST be rehearsed — an
  availability SLA without a rehearsed restore is a claim, not a commitment.
- **Build for the current stage**: Do not build a later stage's infrastructure
  early. Each phase MUST produce a shippable, testable increment. A plan whose
  scope exceeds the stage it is shipping into MUST record the deferral
  explicitly (an MVP cut line plus what is deliberately postponed), not silently
  claim compliance.

## Development Workflow & Quality Gates

- **Evals as the release gate**: A real eval set (starting ~20 cases) with an
  LLM-as-judge rubric and end-state checks MUST run in CI and gate any prompt,
  tool, model, or skill change. Track pass rates over N runs; hold out grader tests
  the agent cannot edit to prevent spec-gaming. The gate MUST be in place before
  the first behavior-bearing slice ships (Principle IX).
- **Deterministic tests for a non-deterministic dependency**: Correctness tests
  MUST run against a recorded/fake provider rather than a live model, and the
  transcript-hygiene invariant (paired `tool_use`/`tool_result`) MUST be covered
  by property-based tests, not examples alone.
- **SLOs have error budgets**: Every stated availability/latency SLA MUST have a
  corresponding SLO, an error-budget policy, and burn-rate alerting that names the
  runbook it pages to.
- **Ownership is named**: A platform team owns the shared harness; an AgentOps
  function owns SLOs, on-call, evals-in-CI, cost dashboards, and behavioral
  incident response; a governance/risk function signs off new tools, connectors,
  and autonomy levels and maintains the AI risk register. An unowned control is
  not a control.
- **Prompts/tools/skills are reviewed config**: Changes go through version control
  and code review like any release; a governance/risk function signs off new tools
  and autonomy levels.
- **Observability captures structure, not content**: Decision patterns and per-turn
  cost/latency/token spans MUST be inspectable without reading conversation
  content. This is a property of the pipeline, not a convention: telemetry is a
  **content-free signal class** enforced by a deny-by-default attribute allowlist,
  and no flag may admit content to it — a content-bearing span is an unencrypted
  copy of customer data outside the crypto-shredding boundary, which would render
  an erasure attestation false. The actual prompt/response MUST remain inspectable
  for debugging, but **only** through an audited, scoped, expiring content-access
  grant that emits a hash-chained receipt on grant and on every read under it.
  Reading a customer's conversation MUST NOT be the one privileged operation
  without a receipt in a system where every mutating action has one.
- **State artifacts are not interchangeable**: A model-facing context compaction,
  a machine-facing resume checkpoint, and a disposable projection snapshot are
  three distinct artifacts and MUST NOT be conflated — a compaction cannot answer
  whether an external effect completed. The exactly-once claim for a
  state-changing effect MUST be committed **write-ahead**, before the effect
  leaves the process, and an in-flight claim MUST be resolved on resume by probe
  or by human decision, never by re-execution and never by silent discard.
  "Replay" MUST be three named operations with distinct guarantees — a pure
  projection rebuild, a resume of the same run, and a fork into a new run with
  external effects disabled — and every run MUST persist the digest of the
  configuration that determined its behavior, or it is not reproducible.
- **Go-live gate**: No production launch without the go-live checklist green —
  attributable audit log, vaulted per-tenant secrets, sandboxing + human approval
  for high-impact actions, at least one leg of the lethal trifecta broken per risky
  flow, per-task/per-tenant cost ceilings, failure classification + resume + stuck
  detection, evals green in CI, cache-read >90% steady-state, documented data
  residency/retention/no-train, and a rehearsed behavioral-incident runbook.

## Governance

This constitution supersedes all other development practices. Where a plan, spec,
task list, or code review conflicts with it, the constitution wins and the conflict
MUST be resolved before merge.

- **Amendments** MUST be proposed in writing with rationale, reviewed by the
  platform and governance/risk functions, and accompanied by any required migration
  plan and template/artifact updates.
- **Versioning** follows semantic versioning: MAJOR for backward-incompatible
  governance or principle removals/redefinitions, MINOR for a newly added or
  materially expanded principle/section, PATCH for clarifications and non-semantic
  refinements.
- **Compliance review**: Every PR and design review MUST verify compliance with the
  Core Principles; the Constitution Check gate in the plan template is the
  enforcement point. Any added complexity MUST be justified against these
  principles.
- **Runtime guidance**: Agent-specific and contributor guidance files are
  subordinate to this constitution and MUST be kept consistent with it.

**Version**: 1.2.0 | **Ratified**: 2026-07-17 | **Last Amended**: 2026-07-30
