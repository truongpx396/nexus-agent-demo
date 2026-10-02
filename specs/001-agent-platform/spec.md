# Feature Specification: Nexus Agent Platform (baseline)

**Feature Branch**: `001-agent-platform` (retroactive — no branch was cut; the work shipped to `main` through PRs #1–#57)

**Created**: 2026-10-02

**Status**: Baseline — describes the behavior shipped at `8e02491`

**Input**: User description: "Nexus Agent platform baseline: a multi-tenant, production-grade AI agent platform — one kernel loop behind thin surfaces, with a governed tool/permission pipeline, pre-spend cost ceilings, a tamper-evident trust surface, durable resume/replay/fork, memory and skills, orchestration, delegation and peer teams, an eval release gate, additional surfaces, retrieval, production hardening and observability."

> **How to read this document.** This project was built from `docs/build-phases.md` (a task ledger) before Spec Kit
> was adopted (#55), so this spec is reconstructed, not original. Three sources of truth were reconciled: the
> ledger's "Proves" column, the constitution (`.specify/memory/constitution.md`), and the code at `HEAD`. Where they
> disagree, the code wins for *what is shipped* and the constitution wins for *what is allowed*; every disagreement is
> listed under [Assumptions](#assumptions) rather than smoothed over.
>
> `[orig: FR-nnn]` tags point at the requirement numbers the ledger and code comments cite from the upstream design
> (`truongpx396/nexus-agent`, whose `specs/001-agent-platform/spec.md` is not vendored here). The requirement *text*
> is reconstructed from how this repo uses each number, so treat a tag as a best-fit cross-reference, not a quotation.
> Requirements without a tag are new to this demo.

## User Scenarios & Testing *(mandatory)*

Actors: an **End user** submits work; an **Operator** (tenant admin) configures a tenant; an **Approver** decides
gated actions; a **Security reviewer** must be able to sign the system; a **Platform engineer** changes prompts, tools
and models; **AgentOps** runs it in production.

Priorities follow the build order, which is also the dependency order: P1 is "a governed, bounded agent", P2 is
"an agent you can trust, recover, extend and safely change", P3 is "more reach". Each story is independently
testable and maps to one phase of `docs/build-phases.md` (shown as *Ledger*).

### User Story 1 - Run an agent task and watch it happen (Priority: P1) 🎯 MVP

An end user submits a task and receives a run they can follow live: model output, tool calls, tool results and a
final outcome, all as an ordered stream. The same task submitted through a different surface behaves identically,
because every surface drives one shared loop.

*Ledger*: Phase 2. **Why this priority**: nothing else has value until a single task can run end to end, and the
loop's invariants (typed outcomes, paired tool results, cache-stable prompts) are the hardest to retrofit.

**Independent Test**: Submit a task against the deterministic provider; observe an accepted run, an event stream
ending in exactly one typed terminal outcome, then kill the process mid-turn and confirm the surviving log still
pairs every tool call with a result.

**Acceptance Scenarios**:

1. **Given** an authenticated end user, **When** they submit a task, **Then** the platform accepts it immediately and
   the run's events stream in order — content, tool calls, tool results, terminal — each carrying its sequence number.
2. **Given** a run in which the model requests two tools and the process dies between them, **When** the run is
   inspected afterwards, **Then** every tool call has exactly one paired result (a synthetic one where the real
   result never arrived).
3. **Given** a multi-turn session, **When** the prompt for turn N+1 is built, **Then** the stable portion is
   byte-identical to turn N's and only the volatile tail differs; a one-byte change to the stable portion fails a test.
4. **Given** the model refuses on policy grounds or its context overflows, **When** the run ends, **Then** the
   outcome is the distinct typed reason (`refused`, `context_overflow`) — never a silent completion and never a failover
   that would re-send the overflowing context.
5. **Given** a model that streams slowly, **When** the client subscribes, **Then** it also receives best-effort live
   preview fragments that never appear in, or alter, the durable record.

---

### User Story 2 - Governed tool use (Priority: P1)

Every tool call — builtin, skill-provided or remote — passes one ordered pipeline and one ordered permission
resolution before it can have an effect. Safety is judged per invocation on the parsed input, defaults to refusal, and
can only ever be tightened by hooks, skills, standing approvals or the user.

*Ledger*: Phase 3. **Why this priority**: an ungoverned agent is a liability; this is the platform's defining control.

**Independent Test**: Run "delete the build directory" at the most restricted autonomy level and at the supervised
level; confirm a typed, audited refusal in the first and a suspension for approval in the second.

**Acceptance Scenarios**:

1. **Given** a run pinned to read-only autonomy, **When** the agent requests a destructive tool, **Then** it is refused
   by the autonomy layer with a typed reason naming the layer, and the refusal is audited.
2. **Given** a standing approval that covers a tool, **When** the call is also unsafe or taint-engaging, **Then** the
   safety and Rule-of-Two evaluations still run — a standing approval satisfies a question, never skips it.
3. **Given** a hook that returns "allow", **When** the chain resolves, **Then** the answer is treated as "no
   opinion"; a hook can deny, ask or defer but never grant.
4. **Given** two tools claiming the same namespace, **When** the catalog is admitted, **Then** the collision is
   refused at admission, regardless of registration order.
5. **Given** a session that has engaged two of {untrusted input, private data, external effect}, **When** a call would
   engage the third, **Then** a human approval is required, and the engaged set survives a process restart.

---

### User Story 3 - Costs that cannot run away (Priority: P1)

An operator sets per-task and per-tenant ceilings. The platform reserves the worst-case cost of a model call *before*
making it, so a run stops before overspending rather than reporting an overrun afterwards.

*Ledger*: Phase 4. **Why this priority**: cost is the primary stop signal; step counts are only a backstop.

**Independent Test**: Set a five-cent per-task ceiling; the run ends `cost_exhausted` before the call that would
exceed it, with a decision naming the budget that refused. Then fire 20 concurrent sessions at one tenant ceiling and
confirm aggregate spend never exceeds it.

**Acceptance Scenarios**:

1. **Given** a per-task ceiling, **When** the next call's worst case would exceed it, **Then** the run terminates
   `cost_exhausted` and records which budget refused.
2. **Given** a provider that fails after streaming begins with no usage reported, **When** the call is reconciled,
   **Then** it is charged at the full reserved worst case and flagged — an unreliable provider never looks free.
3. **Given** the shared counter's epoch is unknown (cache lost), **When** a reservation is attempted, **Then** it
   fails closed rather than assuming nothing was spent.
4. **Given** any auxiliary model call (compaction, safety classification, prompt hook, judge, title, embedding),
   **When** it is made, **Then** it was reserved against the same gate — "off the paying loop" means a cheaper model,
   never an unmetered one.

---

### User Story 4 - A trust surface a security review survives (Priority: P2)

A security reviewer can verify, not just trust, that actions are attributable, tamper-evident and erasable. High-impact
actions suspend for a named human whose decision is bound to the exact arguments they saw.

*Ledger*: Phase 5. **Why this priority**: it is what makes the platform signable, but it needs the P1 loop to exist.

**Independent Test**: Run the "email the Q3 numbers" flow: it suspends; the approver sees recipient/subject/attachment
digests (never a bare identifier); modifying the recipient at grant time executes the approver's value; substituting an
argument after the grant yields a typed mismatch. Then erase the tenant and confirm replay and chain verification still pass.

**Acceptance Scenarios**:

1. **Given** a mutating action, **When** it completes, **Then** a hash-chained, signed receipt exists whose signing key
   the writing process cannot read, and the verifier reports a break or a sequence gap if one is introduced.
2. **Given** a granted approval, **When** the arguments differ at resume time, **Then** the run receives a typed
   `approval_mismatch` and is never silently re-asked.
3. **Given** an approval that expires, **When** the deadline passes, **Then** the *action* is denied with a typed
   result the run can replan around; the run is not necessarily terminated.
4. **Given** an erasure request, **When** it completes, **Then** the content is unrecoverable, derived artifacts are
   gone in the same transaction, and the event sequence, digests and audit chain still verify.
5. **Given** a support engineer needing to read a conversation, **When** they request access, **Then** access is only
   via an audited, expiring grant that emits a receipt on the grant and on every read.
6. **Given** injected attempts to simulate consent, widen autonomy mid-run, or reach a gated effect through a standing
   approval, **When** they are made, **Then** every one is refused and audited.
7. **Given** tool code running in the sandbox, **When** it exceeds a CPU, memory, process or wall-clock limit, or tries
   to reach the network, **Then** it is terminated or denied, and the sandbox is reclaimed.

---

### User Story 5 - A run that survives failure (Priority: P2)

A worker can die mid-run and the work resumes from the last checkpoint. Failures are classified before any retry,
retries are logged, and a stuck agent is stopped. An engineer can replay, resume or fork a past run.

*Ledger*: Phase 6, plus the durable queue, conversational sessions and durable taint added after the ledger.
**Why this priority**: long runs are only useful if they are recoverable.

**Independent Test**: Hard-kill a worker mid-tool-call; the job is reclaimed, resumes from its checkpoint, and the
in-flight effect claim is escalated rather than re-executed. Then fork the run at a past point with a different model.

**Acceptance Scenarios**:

1. **Given** an effect claim recorded before the effect left the process, **When** the run resumes and finds the claim
   in flight, **Then** it is resolved by probing the outside world or asking a human — never by re-running the effect.
2. **Given** the same failing call repeating, **When** the third identical failure occurs, **Then** the circuit breaks;
   every retry before it was logged with a reason and a jittered delay.
3. **Given** repeated actions, oscillation or zero net change, **When** detected, **Then** the run is marked
   `stuck_suspected` (non-terminal) and only a second corroborating signal terminates it.
4. **Given** a past run, **When** a fork is requested from sequence N with an override, **Then** a new run starts with
   external effects disabled, no inherited approvals, its own budget and audit chain, and the response says whether the
   behavior-determining configuration diverged.
5. **Given** a conversational session awaiting the next message, **When** a message arrives, **Then** the *same*
   session resumes — one audit trail, one ceiling, one taint state — and an abandoned one ends `idle_timeout`.

---

### User Story 6 - An agent that grows: memory, skills and surfaces (Priority: P2)

An operator gives a tenant durable memory and vetted skills. A second real surface (the CLI) proves that adding a
surface requires no change to the loop.

*Ledger*: Phase 7. **Why this priority**: extensibility without weakening any P1/P2 control.

**Independent Test**: Submit the same task through REST and the CLI and compare event sequences; `git diff kernel/` for
the whole phase is empty. Activate a skill, revoke one of its declared tools mid-session, activate again — the revoked
entry is ignored and recorded, and the run continues on the tools it still holds.

**Acceptance Scenarios**:

1. **Given** stored memory, **When** a session starts, **Then** it is injected once at the start after injection
   screening, and a write made during the session takes effect from the *next* session.
2. **Given** a skill bundle, **When** it is admitted, **Then** it is content-addressed and signed, every file is
   scanned, and a bundled script is either registered as a real tool through the ordinary gates or the bundle is refused.
3. **Given** a skill declaring a tool the tenant does not hold, **When** it activates, **Then** that entry is ignored
   and recorded — a skill can never widen capability.
4. **Given** a surface that cannot render approval context, **When** an approval is routed, **Then** routing filters on
   the surface's declared capabilities.
5. **Given** a thread opened by one person and a turn submitted by another, **When** the turn runs, **Then** authority
   is the submitting person's, never inherited from the thread opener.

---

### User Story 7 - Processes, not just conversations: plans and delegation (Priority: P2)

An operator authors a declarative plan whose routing costs no model tokens. An agent can delegate to a sub-agent
within strictly narrower authority and bounds.

*Ledger*: Phase 8. **Why this priority**: multi-step business processes need deterministic routing and safe delegation.

**Independent Test**: A five-step "triage → draft → approve → send → record" plan runs twice, takes the same branch both
times, names the predicate that fired, and uses zero routing tokens. A child that touched untrusted input cannot return a
clean-looking summary.

**Acceptance Scenarios**:

1. **Given** a plan, **When** it is validated, **Then** schema, reachability, bounded loops, closed predicates,
   per-step scope subset and *oversight completeness* are all checked — a plan that routes around its tenant's
   approval policy fails.
2. **Given** a plan version, **When** it is enabled, **Then** it must have passed validation, the eval gate and
   governance sign-off, its agent and route are pinned, and it is thereafter immutable.
3. **Given** a delegation request, **When** the parent's scope is narrower than the grant, **Then** it is refused;
   scope descends as a provable subset and never widens.
4. **Given** a child that read untrusted input, **When** it returns, **Then** the parent's taint folds in the child's own
   event-derived taint, not anything the child claims in its return value.
5. **Given** a fan-out step, **When** it starts, **Then** its worst-case cost is reserved as one envelope before the
   first child starts, and children draw from it.
6. **Given** depth, concurrency or per-run limits, **When** one would be exceeded, **Then** the delegation is refused
   with a non-retryable `bound_exceeded`.

---

### User Story 8 - An agent you can safely change: the eval gate and go-live (Priority: P2)

A platform engineer changes a prompt, tool, model, skill, plan or team roster and the release gate says whether it is
safe to ship, using trial statistics rather than a single run. AgentOps verifies a deployment against a go-live checklist.

*Ledger*: Phase 10 (with the harness skeleton in Phase 1). **Why this priority**: governs every later change.

**Independent Test**: Change a prompt so quality holds but tokens rise 40% — the gate blocks. `make eval` prints a
per-case table with intervals and a three-valued verdict.

**Acceptance Scenarios**:

1. **Given** the corpus, **When** the gate runs, **Then** cases are classed regression / capability / safety / negative
   with distinct thresholds, and safety admits no threshold below 100%.
2. **Given** k trials per case, **When** compared with the baseline, **Then** a regression is a separation of
   confidence intervals, not one flipped trial, and `inconclusive` never resolves to `pass`.
3. **Given** two runs with different evaluation environments, **When** compared, **Then** the gate refuses.
4. **Given** a model-graded criterion, **When** it would block a change, **Then** its judge is a pinned, cross-family
   snapshot already calibrated against human labels, and a held-out grader set the agent cannot reach is measured.
5. **Given** a deployment, **When** the go-live check runs, **Then** each automatable item is verified against it and
   each manual item is printed as an explicit reminder, never a silent pass.

---

### User Story 9 - Safe to put in front of real traffic (Priority: P2)

An operator can deploy the platform, authenticate real principals, shut it down cleanly, know whether it is ready,
and trust that misconfiguration fails loudly.

*Ledger*: Phases 13–15 (closing `docs/production-readiness-review.md` F1–F15). **Why this priority**: the architecture
is only valuable if the binary can be exposed at all.

**Independent Test**: Start from images with no environment configuration outside dev mode — it exits with one error
listing every missing setting. A request with no token is rejected; readiness is true only once every dependency answers.

**Acceptance Scenarios**:

1. **Given** a request without a valid bearer credential, **When** it arrives, **Then** it is rejected before any run
   logic; the principal comes only from verified claims, never from a client-supplied header.
2. **Given** a termination signal, **When** received, **Then** in-flight work drains and every background loop stops.
3. **Given** a dependency outage, **When** readiness is probed, **Then** it reports not-ready while liveness stays up.
4. **Given** sandboxed tool containers, **When** inspected, **Then** capabilities are dropped, the root filesystem is
   read-only, privilege escalation is blocked and the user is non-root.
5. **Given** a routed model, **When** a call is priced, **Then** it uses that model's real rate, including a distinct
   cache-read rate.

---

### User Story 10 - Peer agent teams on a shared board (Priority: P3)

A fixed roster of agents collaborates through a shared task board, claiming cards without ever double-claiming, under one
shared budget.

*Ledger*: Phase 9. **Why this priority**: new scope beyond the original design; valuable but not foundational.

**Independent Test**: Spin up a three-member team on a six-card board; two members race for one card and exactly one
wins; a clean member reading a card written under a tainted session becomes tainted; total spend stays within the
envelope reserved at creation.

**Acceptance Scenarios**:

1. **Given** a roster, **When** a team is created, **Then** the roster is fixed, members are ordinary sessions, and no
   member can delegate or create a nested team.
2. **Given** a card body, **When** it is written, **Then** it is injection-scanned; a flagged card is never surfaced to
   another member.
3. **Given** a tainted card, **When** a clean member reads it, **Then** the reader's taint picks up the untrusted leg.
4. **Given** the board empties and every member is terminal — or the envelope exhausts, or a wall-clock backstop trips —
   **Then** the team ends and every still-active member is reaped.

---

### User Story 11 - More reach: MCP, connectors, messaging, scheduling and a web app (Priority: P3)

The same task works from Telegram, Zalo, email, a scheduler and a browser chat UI, and remote MCP tools and per-user
OAuth connectors are admitted exactly as strictly as builtin tools.

*Ledger*: Phase 11, plus session continuity and the chat UI added after the ledger.
**Why this priority**: each is another instance of the surface seam, built once a real integration need exists.

**Independent Test**: The same task via Telegram, email and the web app yields the same events and terminal reasons as
REST. An MCP tool is admitted, then denied for a tenant whose profile excludes it, audited like a builtin refusal.

**Acceptance Scenarios**:

1. **Given** an inbound webhook, **When** it arrives, **Then** provider authenticity is verified, replays outside a
   bounded window are rejected and the sender is rate-limited *before* the loop sees it. *(Shipped: authenticity and a
   per-tenant limit. Not shipped: the replay window and per-sender limiting — Known deviation 5.)*
2. **Given** a returning chat participant, **When** they message again, **Then** the same conversation resumes via a
   deterministic per-peer key, and the model's reply — not only approval requests — is delivered back.
3. **Given** a stored OAuth credential, **When** a tool runs, **Then** the token is usable only inside the tool's call and
   appears in no event payload, log line or span.
4. **Given** a due schedule, **When** it fires, **Then** a synthetic scheduler principal submits the run through ordinary
   queue admission.

---

### User Story 12 - Durable knowledge beyond memory: retrieval (Priority: P3)

A tenant ingests documents and the agent retrieves ranked, source-attributed passages through an ordinary governed,
metered tool call.

*Ledger*: Phase 12. **Why this priority**: only warranted once file-first memory is exhausted.

**Independent Test**: Ingest a large PDF, retrieve, then erase the tenant — the index is empty and reconciliation finds
nothing outstanding.

**Acceptance Scenarios**:

1. **Given** a document, **When** it is ingested, **Then** it is converted, digested, chunked on stable boundaries and
   scanned before any chunk is indexed (fail closed).
2. **Given** a retrieval, **When** it runs, **Then** it is permission-gated, budgeted, tenant-isolated and its chunks are
   screened before entering the prompt.
3. **Given** an erasure, **When** it completes, **Then** retrieval rows are gone in the same transaction.

---

### User Story 13 - Real capability behind the seams: crawling and stronger isolation (Priority: P3)

An end user can have the agent read JavaScript-rendered pages and run code in a stronger isolation backend, selected by
configuration, with every control unchanged.

*Ledger*: Phase 16. **Why this priority**: proves the seams by filling them with real services.

**Independent Test**: Start the opt-in stack and run the web-research flow end to end; with the stack down, the shell
tool still runs and a warning is logged.

**Acceptance Scenarios**:

1. **Given** a crawl, **When** it runs, **Then** it declares the identical taint and obeys the identical egress allowlist as
   the plain fetch tool.
2. **Given** an unreachable isolation backend, **When** a shell call runs, **Then** it falls back with a warning, never a
   hard failure.
3. **Given** the suggested prompts in the web app, **When** clicked, **Then** they exercise crawl, sandboxed shell, skill
   activation and delegation without typing.

---

### User Story 14 - See what the platform is doing (Priority: P3)

AgentOps gets dashboards, alerts, logs, per-container resource use, traces and profiles from an opt-in stack that costs
nothing when not started.

*Ledger*: Phase 17, plus tracing and profiling added after the ledger.
**Why this priority**: operability, but strictly additive.

**Independent Test**: Start the stack; the metrics, container and name-mapping targets report healthy within one scrape
interval, four dashboards and three datasources load with no manual setup, and stopping the core triggers the
target-down alert.

**Acceptance Scenarios**:

1. **Given** any run, **When** metrics are scraped, **Then** golden signals and a per-tool / per-model cost breakdown are
   exposed, with money converted for display only at the exposition boundary.
2. **Given** the core services stopped, **When** the optional stack is inspected, **Then** it is unaffected, and vice versa.
3. **Given** more than one tracing destination, **When** a run completes, **Then** the same run appears in each with
   correct nesting.

---

### Edge Cases

- A tool result larger than the budget: it is capped and paginated, the overflow spills to a blob store, and the model
  sees a preview with an explicit "do not infer success from the preview" banner.
- A stream truncated mid-tool-call, malformed, stalled or throttled: each is a typed classification, not a guess.
- The signing service is unreachable: no event can be appended — the platform fails closed rather than appending
  unreceipted events.
- Concurrent runs in one session: serial per session, concurrent across sessions.
- A steer message arrives for a run suspended on approval: the approval is invalidated and a paired synthetic result
  released.
- An approval's target surface cannot render approval context: routing filters on the surface's declared capability.
- A skill, tool descriptor or plan changes between admission and use: the digest is re-verified at use.
- The price book has no entry for a routed model or meter: the reservation fails closed.
- A webhook is replayed or arrives for a different tenant than its secret: it is rejected before reaching the loop.
- A conversational session is abandoned: it ends `idle_timeout`, distinct from a human cancel.
- A team member tries to delegate or nest a team: refused — members are leaves.

## Requirements *(mandatory)*

### Functional Requirements

#### Foundations (cross-cutting; phases 0–1)

- **FR-001**: Tenant identity MUST be the first dimension of every session key, stored record, workspace path, cost record
  and secret. [orig: Principle VI]
- **FR-002**: Cross-tenant data isolation MUST be enforced by the data store itself on every tenant-scoped record,
  including for the role that owns the tables; application-level checks alone are insufficient. [orig: FR-039]
- **FR-003**: Tenant scope MUST be transaction-local and MUST NOT be session-level. Isolation MUST be proven by a test
  that runs through the same connection-pooling tier production uses, and that test MUST fail if scope is made
  session-level. [orig: FR-039]
- **FR-004**: Conversation state MUST change only by appending typed events to an append-only log; updates and deletes
  MUST be rejected for every role. [orig: FR-040]
- **FR-005**: Every event MUST carry a schema version, a digest over its plaintext, a key identifier, a tool-pair
  reference and a trace/span join key, and a documented upcasting path MUST keep historical events replayable.
  [orig: FR-006, FR-085, FR-086]
- **FR-006**: Derived state (session status, terminal reason, taint state, durations) MUST be a projection rebuilt by
  replay, never a second source of truth; a test MUST assert rebuilt equals stored. [orig: FR-086]
- **FR-007**: Event content MUST be sealed under a per-tenant key; the plaintext digest MUST survive key destruction.
  [orig: FR-080, FR-089]
- **FR-008**: A deterministic scripted provider MUST exist that can reproduce truncation, stall, malformed stream,
  throttling and failover, and no correctness test MAY call a live model. [orig: FR-097]
- **FR-009**: Telemetry MUST be content-free by a deny-by-default attribute allowlist enforced at the exporter, with a
  test that supplies every content-bearing key. [orig: FR-117]
- **FR-010**: Every run MUST persist a digest of all behavior-determining configuration (prompt version, tool catalog
  manifest, skill set, safety and approval policy, prompt mode) at start, and it MUST NOT change mid-run. [orig: FR-129]
- **FR-011**: The evaluation corpus, runner, code graders and CI gate MUST exist before the first behavior-bearing
  capability ships. [orig: Principle IX]
- **FR-012**: The control-plane / data-plane split and every other import boundary MUST be enforced by an automated test
  over the transitive import graph. [orig: Delivery constraint]
- **FR-013**: Schema changes MUST be expand-only; a rollback is a new later migration, never an edit.
- **FR-014**: Non-test source files MUST NOT exceed 500 lines, enforced by test.

#### Run lifecycle and the kernel (US1)

- **FR-015**: One kernel loop MUST power every surface; surfaces MUST NOT fork or re-implement control flow, and adding
  a surface MUST change no kernel code. [orig: FR-001]
- **FR-016**: The loop MUST classify every model response as tool calls, content or empty and dispatch on the
  classification, never on response text. [orig: FR-002]
- **FR-017**: Every run MUST end under exactly one typed terminal reason, each with a named producer, and every switch
  over reasons MUST be exhaustive. The shipped reasons are `completed`, `max_turns_exceeded`, `cost_exhausted`,
  `aborted`, `stuck_terminated`, `permission_denied`, `context_overflow`, `error`, `refused` and `idle_timeout`. [orig: FR-004]
- **FR-018**: Every tool use MUST have exactly one paired tool result — synthetic on any cancel, error or crash path —
  before the next model call, verified by a property test over generated histories. [orig: FR-003, FR-097]
- **FR-019**: Before each model call the loop MUST drop orphan results, backfill missing synthetic results and prune
  stale observations. [orig: FR-060]
- **FR-020**: The prompt MUST be a byte-stable prefix (stable system prompt, sorted resident catalog, append-only
  transcript) plus a volatile tail; the system prompt MUST NOT change mid-session. [orig: FR-013]
- **FR-021**: Cache-read MUST be computed from recorded per-class token counts and MUST reach at least 90% on
  steady-state turns; the request MUST mark a cache breakpoint closing the stable portion. [orig: FR-014, SC-003]
- **FR-022**: All model access MUST go through one provider abstraction with one normalized stream, native tool-calling
  only, usage split by token class, and typed content blocks that carry tool-use identifiers to the wire. [orig: FR-027]
- **FR-023**: Model routing MUST be deterministic from data label and difficulty, and the decision and reason MUST be
  persisted on the session; regulated payloads MUST be routable to a self-hosted model. [orig: FR-037]
- **FR-024**: Provider failures MUST be classified retryable, permanent or context-overflow; a stream MUST be committed
  after its first chunk, and context overflow MUST NOT be failed over. [orig: FR-167]
- **FR-025**: A provider policy refusal MUST be distinct from a completion and terminate as `refused` with its category;
  model reasoning MUST be round-tripped to the provider and never shown to a client.
- **FR-026**: The REST surface MUST accept a run asynchronously, return its state, stream its events audience-gated, and
  list the caller's sessions. [orig: FR-031, FR-191]
- **FR-027**: Every request MUST be authenticated; the calling tenant and user MUST come only from verified credential
  claims, never from a client-supplied identifier. [orig: F1]
- **FR-028**: A best-effort live preview channel MAY carry fragments as they are produced; it MUST be decoupled from the
  durable log and never read by cost, dispatch or audit, and reasoning content MUST be stripped from it.
- **FR-029**: A session MAY be opted into conversational mode, in which a plain reply pauses it (`awaiting_input`) rather
  than ending it, a later message resumes the same session, and an abandoned one ends `idle_timeout`.

#### Governed tool use (US2)

- **FR-030**: A tool's identity MUST be `{namespace}/{name}@{version}` with one owner per namespace; a collision MUST be
  refused at admission, never resolved by registration order. [orig: FR-147]
- **FR-031**: A catalog manifest MUST pin the resolvable tool universe into the configuration digest, including when
  disclosure is deferred. [orig: FR-148]
- **FR-032**: Each tool descriptor MUST pass an injection scan (`pending → clean | flagged | rejected`, fail closed) at
  admission and MUST be re-verified against its pinned digest at use. [orig: FR-113]
- **FR-033**: Every tool invocation MUST pass one ordered sixteen-step pipeline with each step individually tested.
  [orig: FR-007, FR-010]
- **FR-034**: A canonical digest over the tool and its input MUST serve as approval binding, idempotency key and
  resume-time re-verification. [orig: FR-103, FR-071]
- **FR-035**: Permission resolution MUST be one published ten-layer total order in which a deny is final, no bypass mode
  exists, and the safety and Rule-of-Two layers are always evaluated, covered by a table-driven test over the layer
  cross-product. [orig: FR-111]
- **FR-036**: Autonomy MUST be a one-way ratchet pinned at run start; no operation that widens it MAY exist. [orig: FR-111]
- **FR-037**: Tool profiles MUST be versioned tenant configuration and MUST resolve only to deny or defer. [orig: FR-176]
- **FR-038**: Safety MUST be classified per invocation by in-process rules and then a model leg with a bounded timeout
  and circuit breaker, failing closed to ask. [orig: FR-009, FR-116]
- **FR-039**: Every tool MUST declare which Rule-of-Two legs it engages (defaulting to all); session taint MUST be a
  projection of `taint_transition` events that survives restart, and only an operator re-baseline MAY clear the
  untrusted leg. [orig: FR-033, FR-087]
- **FR-040**: Hooks MUST be tighten-only (an "allow" is a defer), time-bounded with a default of block, budgeted per chain
  and per turn, cached, SSRF-guarded where remote, and any input rewrite MUST pass a path allowlist and re-bind the
  digest. [orig: FR-171, FR-166]
- **FR-041**: Builtin tools (file read/write/search, shell, web fetch, clarification) MUST declare taint and effect
  class. [orig: FR-056–FR-059]
- **FR-042**: A tool result over the budget MUST be capped and paginated with overflow spilled to a blob store and a
  preview that warns against inferring success. [orig: FR-010]

#### Cost governance (US3)

- **FR-043**: Money MUST be exact integers with an explicit currency, rounded once at a declared boundary; no binary
  floating point MAY appear in the cost path. [orig: FR-180]
- **FR-044**: A meter registry MUST define `(meter, quantity, unit)` with a reservable flag; token meters are emitted and
  others registered but unemitted. [orig: FR-179]
- **FR-045**: A versioned price book keyed by meter, subject and effective range MUST keep historical cost reproducible
  and price each routed model at its own rate, including a distinct cache-read rate. [orig: FR-084, FR-181]
- **FR-046**: Every model call MUST reserve its worst case against an atomic per-tenant counter before it is made and
  reconcile afterwards; an unknown counter epoch MUST be treated as unavailable and fail closed. [orig: FR-083, FR-186]
- **FR-047**: A worker-local hard per-run ceiling MUST be enforced synchronously, never depending on a round trip.
- **FR-048**: Every gate resolution — allow, refuse, degrade and skip — MUST be recorded with its reason, resolved scope
  and deciding budget, so an unenforced ceiling is distinguishable from one with room. [orig: FR-188, FR-190]
- **FR-049**: Unreported usage MUST reconcile at the full reserved worst case and be flagged. [orig: FR-185]
- **FR-050**: Every model call — including compaction, safety leg, prompt hooks, judge, titles and embeddings — MUST pass
  through the gate, enforced by a static check. [orig: FR-165]
- **FR-051**: Cost records MUST be appended in the same transaction as the turn and shipped through an outbox. [orig: FR-124]

#### Trust surface (US4)

- **FR-052**: Each mutating action MUST produce a receipt hash-chained per session over digests (so redaction never
  breaks verification) and signed by a custodian whose key the writing process cannot read. [orig: FR-040, FR-081]
- **FR-053**: The chain head MUST be periodically anchored outside the writing system and a scheduled verifier MUST alert
  on a break or sequence gap. [orig: FR-081]
- **FR-054**: Erasure MUST destroy the tenant key (crypto-shredding), hard-delete derived artifacts in the same
  transaction, run a reconciliation proving none outlived its source, and leave replay and chain verification intact.
  [orig: FR-080, FR-162]
- **FR-055**: An approval MUST be a transaction bound to the canonical digest, carrying a decision-ready context package
  (never a bare identifier), a named assignee, a TTL and outcomes `granted`, `granted_modified`, `denied`, `expired`
  and `invalidated`. [orig: FR-036, FR-103–FR-108]
- **FR-056**: A digest mismatch at resume MUST yield a typed `approval_mismatch`, never a silent re-request; a modified
  grant MUST execute the approver's input and the agent MUST NOT be told it ran unmodified. [orig: FR-103]
- **FR-057**: Suspension for approval or input MUST cost zero model tokens, and the suspended interval MUST be excluded
  from every latency measure. [orig: FR-036, FR-120]
- **FR-058**: An input request MUST be schema-declared, resolve on expiry to a recorded default or `input_expired`, and
  carry zero authorization value. [orig: FR-110]
- **FR-059**: Cancel, terminal, reap, ceiling breach and steer-into-suspension MUST invalidate pending approvals and
  release a paired synthetic result. [orig: FR-106]
- **FR-060**: Plaintext content outside the run's own audience MUST be reachable only through an audited, expiring,
  scoped grant that emits a receipt on the grant and on every read. [orig: FR-118]
- **FR-061**: The sandbox MUST impose hard CPU, memory, process and wall-clock limits, default-deny the network, scope a
  workspace per session, terminate and reclaim on breach, and run unprivileged with dropped capabilities and a read-only
  root. [orig: FR-047, FR-059]
- **FR-062**: Web fetch egress MUST be allowlisted, and connector and MCP endpoints MUST be in the sandbox deny set.
  [orig: FR-149]
- **FR-063**: Injected attempts to simulate consent, widen autonomy mid-run, or reach a gated effect through a standing
  scope MUST be refused and audited. [orig: FR-112, SC-025]

#### Reliability and the three state artifacts (US5)

- **FR-064**: Every run MUST be a durable job on a queue drained by stateless workers anywhere; routing MUST be
  per-session-serial and cross-session-concurrent, and an abandoned lease MUST be reclaimable. [orig: FR-046]
- **FR-065**: A checkpoint MUST capture covered sequence, open claim, held reservation, sandbox handle, pending approval
  digest, in-flight provider request, open delegations and the configuration digest. [orig: FR-024, FR-126]
- **FR-066**: A snapshot MUST be a disposable projection cache — deleting every snapshot MUST change nothing but
  hydration time. [orig: FR-126]
- **FR-067**: A condensation MUST be model-facing only and MUST NOT be able to answer whether an external effect
  completed. [orig: FR-015, FR-130]
- **FR-068**: An effect claim MUST be written `in_flight` before the effect leaves the process; `completed` short-circuits;
  resume MUST resolve an in-flight claim by probe or human, never by re-execution or silent discard. [orig: FR-127]
- **FR-069**: Failures MUST be classified before any retry; retries MUST log a reason with jittered backoff; an identical
  failure MUST circuit-break at the third. [orig: FR-023]
- **FR-070**: Stuck detection MUST mark `stuck_suspected` (non-terminal) and terminate only on a corroborating second
  trip. [orig: FR-115]
- **FR-071**: Run control MUST provide steer (drained at a turn boundary under the serial lock), cancel (the sole
  producer of `aborted`), resume and autonomy tightening. [orig: FR-005]
- **FR-072**: Replay MUST be pure (no model call, tool call or append); fork MUST start a new run from a sequence with
  declared overrides, external effects disabled, no inherited approvals, its own budget and audit chain, and report
  configuration-digest divergence. [orig: FR-128, FR-129]

#### Memory, skills, surfaces (US6)

- **FR-073**: Memory MUST be file-first per tenant, injected once at session start after injection and exfiltration
  screening, retention-bounded (default 90 days), with writes taking effect next session. [orig: FR-019]
- **FR-074**: Memory consolidation MUST be an ordered, metered stage whose durable write precedes the compaction that
  would discard its source, degrading to a no-model extractive pass at a ceiling. [orig: FR-165]
- **FR-075**: A skill MUST be a signed, content-addressed bundle with a per-file scan and three disclosure tiers; a
  bundled script MUST register as a real tool through the ordinary gates or the bundle is refused. [orig: FR-020, FR-151]
- **FR-076**: A skill's declared tools MUST intersect the currently resolved catalog, never union; an absent entry MUST be
  ignored and recorded (`skill_capability_ignored`). [orig: FR-153]
- **FR-077**: Skill activation and reference reads MUST be ordinary tools through the same pipeline and require no
  kernel change. [orig: FR-151]
- **FR-078**: Live pruning MUST be non-destructive and never mutate a logged event; structured compaction MUST run at
  about 80% of budget on a cheaper model through the same provider port, metered. [orig: FR-164, FR-015, FR-130]
- **FR-079**: Each surface MUST declare a capability descriptor, pass a conformance test, and approval routing MUST filter
  on declared capability. [orig: FR-155]
- **FR-080**: Authority MUST be the turn-submitting principal, never inherited from the thread opener. [orig: FR-156]
- **FR-081**: Outbound delivery MUST append its event before sending, be at-least-once and idempotent on
  `(session, sequence, surface, recipient)`, and keep `failed_permanent` distinct from unanswered. [orig: FR-157]
- **FR-082**: The CLI MUST be a second real surface over the same kernel. [orig: FR-001]

#### Orchestration and delegation (US7)

- **FR-083**: A declarative plan MUST consist of steps of kinds agent, delegate fan-out, approval gate, preauth, input
  request, condition and loop, with explicit transitions and a cost envelope. [orig: FR-102]
- **FR-084**: Plan predicates MUST be a closed AST (eq, ne, lt, gt, and, or, in over typed field references) with no
  string evaluation, I/O, model call or unbounded loop, so routing costs zero tokens. [orig: SC-023]
- **FR-085**: Plan validation MUST check schema, reachability, bounded loops, closed predicates, per-step scope subset and
  oversight completeness. [orig: FR-102]
- **FR-086**: A plan version MUST progress `draft → validated → eval_passed → signed_off → enabled`, pin agent version
  and route at enable, and be immutable once enabled; in-flight runs finish on their version. [orig: FR-088, FR-096]
- **FR-087**: The platform MUST evaluate transitions itself, treat a step boundary as a checkpoint and record plan
  events. [orig: FR-024, FR-085]
- **FR-088**: A preauth step MUST enumerate a digest-bound set for one human decision; admitting anything outside its
  enumeration MUST fail validation. [orig: FR-109]
- **FR-089**: Delegation MUST be a tool invocation through the same pipeline — scope descends as a provable subset,
  taint ascends from the child's own event-derived state, and depth ≤ 1, concurrent ≤ 3, per-run ≤ 16 fail closed with a
  non-retryable `bound_exceeded`. [orig: FR-098, FR-099, FR-100]
- **FR-090**: Delegation MUST suspend on the same durable-suspend mechanism approvals use, resuming on return or reap.
  [orig: FR-100, FR-120]
- **FR-091**: Fan-out cost MUST be reserved as one envelope before the first child, and children MUST be reaped when the
  parent ends. [orig: FR-099, FR-100]
- **FR-092**: Root, parent and depth attribution MUST appear on every row. [orig: FR-119]

#### Release gate and go-live (US8)

- **FR-093**: The corpus MUST be classed regression / capability / safety / negative, and safety MUST admit no threshold
  below 100%. [orig: FR-137]
- **FR-094**: The gate MUST run k trials with per-case exact intervals, define regression as interval separation and
  return a three-valued verdict in which `inconclusive` never resolves to `pass`. [orig: FR-138]
- **FR-095**: Results MUST carry an evaluation-environment digest, comparison across digests MUST be refused, and trials
  MUST run on cold sandboxes. [orig: FR-139]
- **FR-096**: Code graders MUST be used wherever a criterion is checkable; a judge MUST be a pinned cross-family
  snapshot calibrated before it may block, and a held-out grader gap MUST be measured. [orig: FR-141]
- **FR-097**: Trajectory (tool selection, ask-versus-guess, turns) MUST be graded, and efficiency (tokens, turns, calls)
  MUST be gated alongside quality. [orig: FR-142, FR-144]
- **FR-098**: Each skill, tool, plan and team roster MUST ship versioned cases run at its promotion gate. [orig: FR-143]
- **FR-099**: CI MUST block any prompt, tool, model, skill, plan or team change below 90% pass or with any regression.
  [orig: FR-042, FR-043]
- **FR-100**: A golden-signal view MUST report completion by terminal reason, ceiling-breach rate, stuck rate, cache-read
  rate, approval time-to-decision, mismatch rate, unresolved in-flight claims, telemetry drop rate and held-out gap.
  [orig: FR-095]
- **FR-101**: A go-live checklist MUST verify each automatable item against a live deployment and print each manual
  item as a named reminder. [orig: FR-045]

#### Production readiness (US9)

- **FR-102**: The server MUST set read, write and idle timeouts, shut down gracefully on signal so every background loop
  stops, and expose liveness separately from readiness (database, queue, consumer group and signer reachable). [orig: F2]
- **FR-103**: The long-running binaries MUST be packaged as self-contained non-root images with a compose profile. [orig: F3]
- **FR-104**: Configuration MUST be validated once at startup and fail closed, with one combined error, on any unset
  security-critical setting outside an explicit dev mode; a missing key path MUST NOT silently generate a key. [orig: F12]
- **FR-105**: CI MUST scan dependencies for vulnerabilities and automate dependency updates. [orig: F11]
- **FR-106**: The highest-consequence packages MUST have unit tests; none under `internal/` MAY report 0.0% coverage.
  [orig: F9]
- **FR-107**: A live-model evaluation track MUST exist behind a build tag, graded by the code graders. [orig: F6]
- **FR-108**: Spans MUST be exportable to a real collector with the filtering guarantee unchanged, and aggregate metrics
  MUST be exposed on a scrape endpoint. [orig: F13]
- **FR-109**: Connection-pool sizing MUST be explicit and reconciled with the pooler, the streaming reader's buffer MUST
  exceed its default, and a load test MUST publish a measured submission rate. [orig: F14]
- **FR-110**: A versioned control-plane contract (`v1` request/response shapes behind one port) MUST exist with a local
  implementation, and its import-boundary rules MUST be active. [orig: F15]

#### Peer teams (US10)

- **FR-111**: A team's roster MUST be fixed at creation, members MUST be ordinary sessions, and a member MUST be a leaf
  that cannot delegate or create a team.
- **FR-112**: Board cards MUST follow `open → claimed → in_progress → done | blocked`, copy taint from the writer, and be
  injection-scanned; a flagged card MUST never be surfaced to another member.
- **FR-113**: Claiming a card MUST be atomic — exactly one claimant wins under contention.
- **FR-114**: Reading a card MUST fold its taint into the reader's own taint.
- **FR-115**: A shared envelope MUST be reserved once at creation, sized to the roster's worst case.
- **FR-116**: A team MUST progress `active → completed | aborted | ceiling_exhausted` and reap every still-active member
  on ending, including at a wall-clock backstop.

#### Additional surfaces and connectors (US11)

- **FR-117**: Remote MCP tools MUST be qualified `mcp/{server}/{tool}@{version}` and admitted through the ordinary
  identity, manifest and descriptor-scan path, resolved per tenant and per calling user.
- **FR-118**: Per-user OAuth credentials MUST be sealed under the tenant key, readable only inside a tool's call and
  never placed in an event payload, log line or span.
- **FR-119**: Connector tools MUST declare taint per action and be gated by the unmodified permission chain.
- **FR-120**: Inbound webhook surfaces MUST verify provider authenticity, reject replays outside a bounded window and
  rate-limit per external identity before the kernel sees the payload.
- **FR-121**: Messaging surfaces MUST resolve a deterministic per-peer session key, resume an awaiting conversation or
  start a fresh conversational one, and deliver the model's reply through the outbox.
- **FR-122**: A scheduler surface MUST submit runs as a synthetic scheduler principal through ordinary queue admission.
- **FR-123**: A web application MUST present sessions, live preview, tool calls, sub-agents, approvals with digests, taint
  state and terminal reasons using only the public API plus additive read endpoints.
- **FR-124**: The surface conformance suite MUST cover every surface.

#### Retrieval (US12)

- **FR-125**: Document ingestion MUST normalize PDF, DOCX, HTML and plain text, compute a deterministic digest and chunk
  on declared, stable boundaries.
- **FR-126**: A converted document MUST pass an admission scan before any chunk is indexed (fail closed).
- **FR-127**: The vector index MUST be tenant-scoped under the same isolation rule as every other table.
- **FR-128**: Embedding calls MUST go through the provider abstraction and the budget gate, with a deterministic fake.
- **FR-129**: Retrieval MUST be an ordinary tool declaring `reads_private_data`, budgeted and paginated, and retrieved
  chunks MUST be screened before entering the prompt.
- **FR-130**: Erasure MUST empty the retrieval index in the same transaction.

#### Agentic capability (US13)

- **FR-131**: A crawl tool MUST return rendered, boilerplate-stripped text under the same taint declaration and egress
  allowlist as the plain fetch tool; delegating the fetch to another service MUST NOT delegate the trust decision.
- **FR-132**: A second isolation backend MUST be selectable by configuration behind the same execution seam, falling back
  with a warning when unreachable.
- **FR-133**: Third-party services MUST be opt-in, additive and independently stoppable without touching core services.
- **FR-134**: Real skill bundles and named delegation conventions MUST be shipped over the unmodified skill and delegate
  mechanisms, with UI suggestions that exercise them.

#### Observability stack (US14)

- **FR-135**: Logs MUST be structured with a configurable level floor and an optional file sink.
- **FR-136**: An opt-in stack MUST provide metrics, logs, alerting and per-container resource use with zero-click
  provisioning of datasources, four dashboards and five alert rules.
- **FR-137**: Per-tool call counts and per-model token and cost breakdowns MUST be exposed, converting money for display
  only at the exposition boundary.
- **FR-138**: Traces MUST be exportable to several destinations simultaneously with correct parent/child nesting.
- **FR-139**: Continuous profiling MUST be opt-in and off by default.

### Key Entities

- **Tenant**: the root of isolation; owns configuration, keys, budgets and every record below.
- **Session (run)**: one agent task; carries surface, user, pinned configuration digest, route, autonomy, lineage
  (root/parent/depth), optional plan and team, and projected status, terminal reason and taint.
- **Event**: an immutable, versioned, sealed fact appended to a session's log; the single source of truth.
- **Tool / Descriptor / Manifest**: a self-describing capability with identity, schemas, effect class, taint
  declaration and an admission verdict; the manifest pins the resolvable set.
- **Approval / Input request**: a durable, digest-bound human decision or question attached to a suspended call.
- **Effect claim**: a write-ahead record that an external effect is in flight, completed or abandoned.
- **Checkpoint / Snapshot / Condensation**: three non-interchangeable state artifacts (resume, disposable cache,
  model-facing summary).
- **Budget / Reservation / Decision / Cost record / Price entry**: the cost-governance chain.
- **Audit receipt / Anchor**: a signed, hash-chained attestation of one event, and a periodic external commitment.
- **Encryption key**: a per-tenant key whose destruction is erasure.
- **Content access grant**: an audited, expiring permission to read a session's plaintext.
- **Sandbox**: a per-session isolated execution environment with limits and a breach record.
- **Delivery**: an outbound message with attempt count and idempotency identity.
- **Plan / Delegation / Fan-out envelope**: declarative process, bounded sub-agent relationship, and reserved cost.
- **Team / Board card / Team envelope**: peer collaboration, shared work item, shared budget.
- **Skill bundle / Memory / Document / Chunk**: governed knowledge artifacts.
- **Surface / Channel / OAuth connection / MCP server / Schedule**: how work enters and how external systems attach.

## Success Criteria *(mandatory)*

### Measurable Outcomes

| ID | Outcome | Story |
|----|---------|-------|
| **SC-001** | After a process is killed at an arbitrary point mid-turn, 100% of tool uses in the surviving log have exactly one paired result. | US1 |
| **SC-002** | A one-byte change to the stable prompt prefix between turns is caught by automated test in 100% of attempts. | US1 |
| **SC-003** | On steady-state turns of a multi-turn session, at least 90% of input tokens are served from cache, measured from recorded per-class counts. | US1 |
| **SC-004** | A destructive request at the most restricted autonomy is refused with a typed, audited reason; the same request at supervised autonomy suspends for approval. | US2 |
| **SC-005** | No standing approval, hook "allow" or skill declaration can skip a safety or Rule-of-Two evaluation or widen capability, shown by exhaustive tests over every layer combination. | US2 |
| **SC-006** | With 20 concurrent sessions against one tenant ceiling, aggregate spend never exceeds the ceiling. | US3 |
| **SC-007** | A run with a per-task ceiling terminates `cost_exhausted` before the call that would exceed it, and the recorded decision names the refusing budget. | US3 |
| **SC-008** | The cost of any past call is recomputable to the exact minor unit from the price version in force at the time. | US3 |
| **SC-009** | A cross-tenant read through the pooled connection tier returns nothing in 100% of attempts, and reverting to session-level scope makes the isolation test fail. | Foundations |
| **SC-010** | After erasing a tenant, no content is recoverable, the event sequence replays structurally and the audit chain verifies. | US4 |
| **SC-011** | The verifier detects 100% of injected tampering (alter, remove, reorder) and sequence gaps. | US4 |
| **SC-012** | An approved action whose arguments change after the grant is refused with a typed mismatch in 100% of attempts, and an approver's modified input is what executes. | US4 |
| **SC-013** | A run suspended for approval or input consumes zero model tokens, and the suspended interval is excluded from every latency measure. | US4 |
| **SC-014** | A test supplying every content-bearing attribute key shows zero leaving the process through telemetry. | Foundations |
| **SC-015** | After hard-killing a worker mid-tool-call, the run resumes from its checkpoint and no in-flight effect is re-executed. | US5 |
| **SC-016** | Deleting every snapshot changes nothing observable except hydration time. | US5 |
| **SC-017** | A fork from any past point runs with external effects disabled and reports configuration divergence. | US5 |
| **SC-018** | The same task through any two surfaces yields identical event sequences and terminal reasons, and adding a surface changes zero kernel lines. | US6, US11 |
| **SC-019** | In 100% of attempts, a skill declaring an unheld tool has that entry ignored and recorded, never granted. | US6 |
| **SC-020** | Every outbound message is either delivered or recorded as permanently failed — none is silently dropped. | US6 |
| **SC-021** | After a delegation returns, the parent's taint always includes the child's own event-derived taint, whatever the child claims. | US7 |
| **SC-022** | No root run ever exceeds depth 1, three concurrent or sixteen total delegations. | US7 |
| **SC-023** | Plan routing consumes zero model tokens: no model call occurs while evaluating a transition. | US7 |
| **SC-024** | The same plan and input run twice take the same branch, and the transition log names the predicate that fired. | US7 |
| **SC-025** | Injected attempts to simulate consent, widen autonomy mid-run or reach a gated effect through a standing scope are refused and audited in 100% of cases. | US4, US8 |
| **SC-026** | A change that holds quality but regresses tokens, turns or tool calls past the declared band is blocked, and an inconclusive result never resolves to pass. | US8 |
| **SC-027** | The gate blocks any prompt, tool, model, skill, plan or team change below 90% pass or with any regression. | US8 |
| **SC-028** | A credential-less request is rejected before any run logic, readiness is true only when every dependency answers, and a startup with missing security-critical configuration exits with one combined error. | US9 |
| **SC-029** | Concurrent claims never double-claim a card, a clean reader of a tainted card becomes tainted, and team spend never exceeds the envelope reserved at creation. | US10 |
| **SC-030** | A scan of all event payloads, log lines and spans finds zero live connector credentials. | US11 |
| **SC-031** | After erasing a tenant, zero retrieval rows remain. | US12 |
| **SC-032** | A static check finds zero model or embedding call sites that bypass the budget gate. | US3, US12 |
| **SC-033** | Optional stacks start and stop without affecting core services, and with them down the dependent core features degrade with a warning rather than fail. | US13, US14 |
| **SC-034** | A measured submission rate (requests per second, deterministic provider) is published rather than assumed. | US9 |

## Assumptions

- **Provenance.** Requirement *numbers* come from the upstream design as cited by `docs/build-phases.md`; their *text* here
  is reconstructed. Before treating an `[orig: …]` tag as authoritative, reconcile it against
  `truongpx396/nexus-agent@2ad2a6a` (`specs/001-agent-platform/spec.md`).
- **Single-binary topology.** The control plane and data plane run in one process behind a versioned in-process contract;
  a physical split is a composition-root change. Three binaries exist: the server, a CLI, and a sign-only key custodian.
- **Deliberately out of scope** (the ledger's four *seams* and six *omissions*): credit ledger, billing periods, FX and
  chargeback; multi-region residency and bring-your-own-key; scheduled adversarial discovery and adaptation proposals;
  third-party integration adapters beyond MCP (port interfaces only); gVisor/Kata isolation classes and a warm sandbox
  pool (the isolation field carries them as unshipped values); the in-sandbox tool broker (a stretch task, not shipped).
  Each has a named trigger in `README.md` §5.
- **Dev defaults.** A scripted provider, auto-generated keys and a development token issuer are available only in an
  explicit dev mode; without it, startup fails closed (FR-104).
- **Users.** Operators configure tenants through the CLI and seed data; there is no operator UI beyond the web chat/approvals app.

### Known deviations (recorded, not resolved here)

These are places where the shipped code, the ledger and the constitution disagree. They are surfaced so a reviewer can
decide; this spec does not pick a side silently.

1. **Content in traces vs. "no flag may admit content" — CRITICAL.** Constitution v1.2.0 (*Observability captures structure,
   not content*) says no flag may admit content to telemetry, and that the only path to plaintext is an audited grant that
   emits a receipt. `NEXUS_TRACE_CONTENT=true` (off by default; `internal/obs/tracer.go`, `docs/local-llm.md`) opens a
   separate span-content channel with no receipt, and its docs call it a "sanctioned exception." Resolution needs either a
   constitution amendment (MINOR, with the receipt requirement stated) or removal of the flag. FR-009 stands as written.
2. **Terminal reasons: 8 vs. 10.** The ledger and `README.md` say eight; the code ships ten (`refused` and `idle_timeout`
   were added later). FR-017 lists the ten; the ledger and README layout comment are stale.
3. **Table count.** `README.md` §3 lists ~31 tables, including `tools`, `tool_profiles`, `catalog_manifests`, `memories`,
   `skills`, `surfaces` and `surface_identities`, that no migration creates (tools, skills and memory are code and files).
   The schema has 32 tables, all with row-level security forced; see `data-model.md`. Related docs drift: the default HTTP
   port is `:8085` in code, compose, CLI and web app, but `CLAUDE.md` and `README.md` say `:8080` and `.env.example` says `:8055`.
4. **Task-number aliases.** Code comments cite tasks 13.8–13.15, which the ledger renumbered into Phases 14–15; the alias
   table is in `research.md`.
5. **Constitution MUSTs with no shipped evidence.** An audit of the code against constitution v1.2.0 found these without
   an implementation; the spec states the requirement (where it has one) and does not claim it is met. Each is a
   Phase 19 task in `tasks.md`:
   - *Inbound webhooks* (FR-120, US11 scenario 1): authenticity is verified before parsing, but there is **no replay
     window**, and the rate limit is keyed per **tenant**, not per external identity.
   - *Egress masking*: no class-based masking of PII, secrets, PHI or card data before content leaves the boundary.
   - *Release gate*: no quality-per-dollar or completions-per-million-tokens metric (efficiency bands only).
   - *Routing*: no capability floor for feature demand.
   - *Supply chain*: dependency scanning exists; no SBOM or artifact signing.
   - *Durability*: no defined or rehearsed backup, restore, RPO or RTO.
   - *SLOs*: alert rules exist but are explicitly uncalibrated; no SLO, error-budget policy or runbook link.
   - *Audit anchor* (FR-053): anchors are append-only and signed, and a ticker verifies them, but they are rows in the same
     database as the receipts; nothing is published to a system outside it, which the constitution requires.
   - *Configuration digest* (FR-010): the digest's safety-policy and approval-policy inputs exist but are **never set**
     by any caller, and the system-prompt version is a hard-coded string, so the pinned digest does not move when safety
     or approval policy changes.
6. **Post-ledger increments.** Conversational sessions, session continuity for messaging surfaces, durable taint state,
   live preview, the stream-backed queue, the chat UI, multi-destination tracing and profiling shipped after the ledger was
   last updated and have no ledger rows; they are specified above (FR-028, FR-029, FR-039, FR-064, FR-121, FR-123,
   FR-138, FR-139) and tasked as Phase 18 in `tasks.md`.
