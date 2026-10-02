# Research: Nexus Agent Platform (baseline)

**Plan**: [plan.md](plan.md) | **Spec**: [spec.md](spec.md)

Retroactive Phase 0. The Technical Context has **no `NEEDS CLARIFICATION`** left — the choices were made while building.
This file records each one as *Decision / Rationale / Alternatives considered*, sourced from `README.md`,
`docs/build-phases.md`, `docs/production-readiness-review.md`, migration and code comments, and commit messages. Where a
rationale is a commit message rather than a design note, the commit is cited so it can be checked.

## Decisions

### R-01 — The kernel loop is a range-over-func generator

- **Decision**: `Kernel.Run` returns `iter.Seq2[store.Event, error]`; control flow lives only there (`kernel/loop.go`).
- **Rationale**: The original's "single async generator" survives translation exactly: the caller pulls events, back-pressure
  and cancellation are the caller's `break`/`ctx`, and a surface cannot interleave its own control flow. Typed terminal
  states fall out of the generator's single exit (FR-015–FR-017).
- **Alternatives**: a goroutine pushing to a channel (leaks on a slow consumer, invites per-surface buffering policy); a
  callback/visitor API (inverts control, so a surface would own sequencing).

### R-02 — One binary; the control/data-plane split is a Go interface

- **Decision**: `internal/controlplane.Port` (v1) with `LocalPort`; packages never import across the boundary, enforced by
  `tests/contract/boundaries_test.go`.
- **Rationale**: Keeps every seam while collapsing topology (README §2). A physical split later replaces `LocalPort` with an
  RPC client in `main.go`. v1 types are deliberately plain data so the wire shape does not move when an internal type does.
- **Alternatives**: three binaries from day one (the original; operational cost with no demo benefit); no boundary at all
  (the state before F15 — the boundary was *documented* and two of its test rules `t.Skipf`'d because the package did not
  exist). The latter is why Phase 15 task 15.4 exists.

### R-03 — Tenant scope is `set_config('app.tenant_id', $1, true)`, behind PgBouncer

- **Decision**: one scoping call, `store.InTenantTx`, transaction-local and parameterised; PgBouncer is **kept** in
  transaction-pooling mode; the isolation test runs through it.
- **Rationale**: `SET LOCAL` cannot take a parameter; `set_config(..., true)` can. Session-level scope is wrong because the
  pooler reassigns a connection between tenants between statements. PgBouncer is kept precisely because the rule is
  meaningless without a transaction pooler to prove it against (README §2).
- **Alternatives**: session-level `SET` (banned by lint and by the test failing when `true`→`false`); per-tenant schemas or
  databases (a topology cost this demo does not pay); application-level `WHERE tenant_id` (the exact failure RLS exists
  to survive).

### R-04 — Two database roles

- **Decision**: `nexus` (superuser, direct to Postgres, migrations only) and `nexus_app` (`NOBYPASSRLS`, through the pooler,
  all runtime) — `migrations/0000_app_role.sql`.
- **Rationale**: RLS is unconditionally bypassed for a superuser regardless of `ENABLE`/`FORCE` (stated in `0001_tenants.sql`).
  `FORCE` still matters for any ordinary owner, so both are applied to every table.
- **Alternatives**: one role (silently disables the control being claimed).

### R-05 — Append-only by trigger *and* privilege

- **Decision**: `events_forbid_mutation` `BEFORE UPDATE OR DELETE` fires for every role including the owner;
  `REVOKE UPDATE, DELETE ... FROM nexus_app` refuses it one step earlier (`0003_events.sql`).
- **Rationale**: Principle II. A convention is not a control; belt and suspenders costs two lines.
- **Alternatives**: grants alone (an owner or superuser still mutates); an application-level guard (bypassed by any other writer).

### R-06 — Envelope encryption per tenant; erasure is crypto-shredding

- **Decision**: KEK (file) wraps a per-tenant DEK; payloads are AES-256-GCM sealed; `payload_digest` is over the plaintext.
  Erasure destroys the DEK (`encryption_keys.shredded_at`) and hard-deletes derived artifacts in the same transaction;
  a reconciliation job proves none outlived its source.
- **Rationale**: Reconciles an append-only log with a right to erasure without rewriting history; because the audit chain is
  over *digests*, a lawful redaction never breaks verification (FR-052, FR-054).
- **Alternatives**: deleting rows (forbidden by R-05, and breaks the chain); per-record MAC only (not tamper-evident —
  constitution says so explicitly); KMS/HSM (deferred; the file KEK sits behind the same interface).

### R-07 — Sign-only audit custody over a unix socket

- **Decision**: `signerd` alone holds the Ed25519 key (`internal/audit/signerkey`, importable only by `cmd/signerd` — a
  contract test); `nexusd` asks it to sign a 32-byte digest over newline-delimited JSON on a unix socket and can read only the
  public half. `nexusd` cannot append an event unless `signerd` answers.
- **Rationale**: A component that writes receipts must not be able to rewrite them. The socket makes framing/multiplexing
  needless; a fresh dial per call is fine (`internal/audit/protocol.go`). Fail-closed on unreachable.
- **Alternatives**: in-process key (writer can forge); HMAC per record (not tamper-evident); a cloud KMS (a dependency the
  demo does not need; the `Signer` interface admits it).

### R-08 — One canonical digest does three jobs

- **Decision**: a *simplified* RFC 8785 (JCS) over `{tool_id, input}` — keys sorted, no insignificant whitespace, strings
  re-escaped — hashing only the tool and its input, never output or a timestamp. The same digest binds an approval, keys
  idempotency, and is re-verified at resume (pipeline steps 5 and 8).
- **Rationale**: Three separate hashes would drift; one artifact makes "the approver saw exactly what runs" a single check.
  Numbers are re-emitted from their original decoded token rather than JCS's ECMA-262 number-to-string rule — *stricter* than
  JCS (byte-identical to the source), which still gives "same input twice, same digest" without JCS's numeric edge cases
  (`internal/tools/digest.go`). The ledger's "RFC 8785" wording therefore slightly overstates it; the divergence is deliberate
  and documented in code.
- **Alternatives**: hashing the raw JSON bytes (key order and whitespace change the digest and spuriously mismatch); full
  JCS number formatting (implementation cost with no benefit to the properties the system needs).

### R-09 — One provider port; a deterministic fake is mandatory

- **Decision**: `provider.Provider.Stream` returns a normalized `Stream` of `Chunk{content|reasoning|tool_use|usage|done}`;
  adapters: `anthropic` (hand-rolled over `net/http`), `litellm`, `fake`. Messages carry typed content blocks end to end (F5).
- **Rationale**: Scattered SDK calls are prohibited (Principle VII). The fake scripts truncation, stall, malformed stream,
  throttle and failover so no correctness test calls a live model (FR-008). Usage is split by token class or the 90%
  cache-read target is unmeasurable (Principle III).
- **Alternatives**: the official SDK (couples the harness to one vendor's types); recording live transcripts only
  (cannot script a stall or a malformed stream on demand).

### R-10 — Redis Streams replaced the Postgres job queue

- **Decision**: `internal/queue.Port` with a Redis Streams consumer-group adapter; `XAUTOCLAIM` reclaims an abandoned lease;
  every run is queued; `migrations/0024_retire_queue_jobs.sql` drops `queue_jobs`; Redis runs with `--appendonly`. A Redis
  Pub/Sub event bus lets an SSE client be served by a different process than the one executing the run. Source: `2efa4ff`.
- **Rationale**: The Postgres adapter wrote `lease_expires_at` on every lease and read it nowhere, so it could not reclaim an
  abandoned job — the swap is a real reliability gain, not a lateral port. Closes the single-process ceiling.
- **Alternatives**: NATS JetStream (the original's choice, still deferred per README §5, trigger: concurrency beyond one
  process's comfort); keep Postgres `SKIP LOCKED` (kept *only* for board-card claims, R-18, where its semantics are right).
- **Ledger impact**: reverses the *adapter* in row #41; the port and the claim are unchanged.

### R-11 — Cost is reserved before the spend, in integer money

- **Decision**: reserve → stream → reconcile against a Redis Lua counter with an **epoch marker** (unknown epoch ⇒
  unavailable ⇒ fail closed), plus a worker-local synchronous per-run ceiling; `Money` is exact integer micros with an explicit
  currency and `forbidigo`/a test ban `float32`/`float64` in `internal/cost`.
- **Rationale**: A ceiling checked by post-hoc aggregation is a lagging indicator (constitution). The epoch prevents "no spend
  yet" being inferred from a lost counter. Conversion to float happens only at the metrics exposition boundary (task 17.9).
- **Alternatives**: per-turn aggregation (overspends under concurrency — SC-006 is the test that proves it); floats (rounding drift).

### R-12 — Docker sandbox now, stronger isolation behind the same seam

- **Decision**: Docker with `--network none` and rlimits, then `CapDrop ALL`, read-only rootfs, `no-new-privileges`, non-root user
  (F10). `NEXUS_SANDBOX=opensandbox` selects a second backend satisfying the same structural `tools.SandboxExec` interface
  (task 16.3). The `isolation` column carries `gvisor|kata` as unshipped values.
- **Rationale**: Pattern #44's own comment names the swap: "a config change later, not a schema or interface change." The
  in-sandbox broker is **not shipped**, so connector and MCP endpoints stay in the egress deny set — a bypass is never the
  interim state.
- **Alternatives**: gVisor/Kata now (needs a runtime class and a warm pool the demo does not have); no sandbox (rejected by FR-061).

### R-13 — Plan predicates are a closed JSON AST

- **Decision**: `eq|ne|lt|gt|and|or|in` over typed field references (`internal/plan/predicate.go`); no string evaluation, I/O, model
  call or unbounded loop.
- **Rationale**: Makes "routing costs zero tokens" a *property of the language*, testable by asserting no `Provider.Stream` call
  occurs during transition evaluation (SC-023).
- **Alternatives**: an expression string language (needs an interpreter, invites I/O and unbounded loops); delegating routing
  to the model (spends tokens and is non-deterministic).

### R-14 — Taint declarations default TRUE; the Rule of Two is layer 7

- **Decision**: every `Tool.Taint()` defaults all three legs `true`; session taint is a projection of `taint_transition` events,
  restored on resume via `Pipeline.SeedTaint` (commit `08a86b7`).
- **Rationale**: Before that commit the state lived only in `Pipeline.sessions`, a process-lifetime map, so a restart silently
  reset every in-flight session's Rule-of-Two enforcement to zero — a hole invisible in normal operation.
- **Alternatives**: persisting the map directly (a second source of truth, contradicting FR-006).

### R-15 — Skills are signed bundles; there is no `skill_search`

- **Decision**: content-addressed `bundle_digest` over every file, per-file scan, three disclosure tiers; `declared_tool_ids`
  *intersect* the resolved catalog; a bundled script is a real tool or the bundle is refused.
- **Rationale**: A skill must never widen capability; the catalog is vetted and per-tenant-bounded, not a corpus, so a search tool
  is unwarranted (a seam is left if that changes).
- **Alternatives**: unioning declared tools (a capability-widening path); executing attachment-borne scripts (an injection path).

### R-16 — Memory is files; retrieval only past the trigger

- **Decision**: file-first memory (`internal/memory`), retrieval (`pgvector`) added in Phase 12 once the ~1M-token trigger fired.
  The demo's embedding column is `vector(32)` (a deterministic fake embedder), not a production dimension.
- **Rationale**: Constitution *Memory is files first*. Retrieval sits *beside* memory and obeys the same screening and tenant
  scoping; erasure must empty it in the same transaction (FR-130) so it is not a durable-knowledge side door.
- **Alternatives**: a vector store from the start (infrastructure before the data shape justifies it).

### R-17 — The eval gate uses trial statistics and a three-valued verdict

- **Decision**: k trials per case, Wilson intervals, regression = interval separation, `inconclusive` never resolves to `pass`,
  a pinned cross-family judge calibrated before it may block, held-out graders, efficiency bands gated, cold sandboxes, and an
  environment digest that forbids cross-digest comparison.
- **Rationale**: A single trial of a non-deterministic system measures the weather. The gate ships in Phase 1 (Principle IX).
- **Alternatives**: pass/fail on one run (flaky and gameable); same-family judge (correlated blind spots).

### R-18 — Board-card claims use their own `SELECT … FOR UPDATE SKIP LOCKED`

- **Decision**: `claim_card` is a Postgres claim query independent of the queue adapter (Phase 9 task 9.4).
- **Rationale**: Exactly-one-claimant under contention is a row-level property, which `SKIP LOCKED` provides; the queue moved to
  Redis (R-10), but the card claim did not need to.
- **Alternatives**: routing claims through the queue (adds a hop and couples two concerns).

### R-19 — Live preview is a second, best-effort channel

- **Decision**: `Kernel.OnChunk` fires per decoded chunk; `cmd/nexusd` fans it out as `event: delta` SSE frames; nothing durable,
  cost-bearing or audited reads it; reasoning bytes are stripped (commit `62f052b`).
- **Rationale**: The durable path buffered a whole turn before appending, so a client saw nothing until the turn ended. The same
  split Anthropic's content-block deltas and LangGraph's messages-vs-checkpoint use.
- **Alternatives**: appending partial content to the log (breaks FR-004's meaning and the paired-result accounting).

### R-20 — Conversational sessions pause instead of terminate

- **Decision**: opt-in `RunConfig.Conversational`; a plain reply sets `awaiting_input`; `POST …/steer` resumes the same session;
  a 30-minute sweep ends an abandoned one `idle_timeout` (commit `2975d08`, `migrations/0023`).
- **Rationale**: Replaced a client-side "chain of sessions" hack that fragmented one conversation across N sessions and
  therefore across N audit trails, N cost ceilings and N taint states.
- **Alternatives**: client-side stitching (what it replaced).

### R-21 — Per-tenant dev token issuer behind a `Verifier` seam

- **Decision**: `rest.PrincipalVerifier` is satisfied by a signed-JWT (Ed25519) dev issuer; the principal comes only from
  verified claims; a nil verifier fails closed (F1, task 13.1).
- **Rationale**: Swapping to a per-tenant OIDC provider is a `cmd/nexusd` wiring change. The previous header-reading path was
  deleted, not deprecated.
- **Alternatives**: trusting `X-Nexus-Tenant-ID` (RLS then protects against bugs, not attackers — the F1 finding).

### R-22 — Observability is additive, opt-in and Docker-project-isolated

- **Decision**: separate compose files and projects for the local LLM, agentic, observability, tracing and profiling stacks;
  cAdvisor pointed at containerd (`-containerd-namespace=moby`) because its Docker factory cannot register containers under the
  containerd-snapshotter backend; a tiny stdlib-only `docker-label-exporter` recovers container names that containerd does not carry.
- **Rationale**: Each stack must be stoppable without touching the core (FR-133, SC-033). The cAdvisor choice is the production
  kubelet mechanism, not a Docker Desktop workaround; the exporter's server-side label filter is also what scopes the dashboard.
- **Alternatives**: a single compose file (a `make down` would take the observability stack with it); relabel-guessing names.

### R-23 — Permissive CORS is acceptable for this API

- **Decision**: `withCORS` reflects the request `Origin`.
- **Rationale**: Auth is a bearer token the client attaches deliberately, never an auto-attached cookie, so there is no CSRF
  exposure; without CORS a browser's preflight gets a bare 405 that surfaces as an opaque "Failed to fetch".
- **Alternatives**: an origin allowlist (reasonable hardening for a real deploy; not required by the threat model here).

## Legacy task-number aliases

Code comments cite `task 13.8`–`13.15`; the ledger later split the original fifteen-task Phase 13 into Phases 13, 14 and 15.
When a comment and the ledger disagree, this table reconciles them.

| Code comment says | Ledger now says | Closes |
|---|---|---|
| 13.1 … 13.7 | 13.1 … 13.7 (unchanged) | F1–F5, F7, F8 |
| 13.8 | **14.1** | F9 — unit tests for the untested packages |
| 13.9 | **14.2** | F6 — live-model eval track |
| 13.10 | **14.3** | F11 — `govulncheck`, Dependabot |
| 13.11 | **14.4** | F12 — validated config, no silent KEK |
| 13.12 | **15.1** | F13 — OTLP exporter, `/metrics` |
| 13.13 | **15.2** | F10 — sandbox hardening |
| 13.14 | **15.3** | F14 — pool sizing, SSE buffer, load test |
| 13.15 | **15.4** | F15 — `internal/controlplane` |

## Production-readiness audit trail (F1–F15)

Source: `docs/production-readiness-review.md`, 2026-09-04. All fifteen are closed by the ledger tasks below.

| # | Finding | Closed by |
|---|---|---|
| F1 | Tenant identity was an unverified header | 13.1 |
| F2 | No server lifecycle: timeouts, shutdown, health | 13.2 |
| F3 | No way to package or deploy the binary | 13.3 |
| F4 | Prompt caching architected and metered but never requested | 13.5 |
| F5 | Tool calls flattened to prose at the provider boundary | 13.4 |
| F6 | The release gate never called a model | 14.2 |
| F7 | Thinking blocks discarded; a refusal read as a clean completion | 13.7 |
| F8 | One wildcard price stood in for three model tiers | 13.6 |
| F9 | Highest-consequence packages had no unit tests | 14.1 |
| F10 | Sandbox containers ran as root with full capabilities | 15.2 |
| F11 | No dependency or supply-chain scanning | 14.3 |
| F12 | Config unvalidated; a missing KEK path generated a new key | 14.4 |
| F13 | No telemetry left the process | 15.1 |
| F14 | Every scale knob at its zero value, nothing measured | 15.3 |
| F15 | The control-plane seam was documented and did not exist | 15.4 |

F1–F15 are not exhaustive. The items under *Known deviations → Constitution MUSTs with no shipped evidence* in the spec
(SBOM, backup/restore, SLOs, egress masking, webhook replay protection, quality-per-dollar, capability-floor routing) were
not raised by the review and are tracked as Phase 19 in `tasks.md`.
