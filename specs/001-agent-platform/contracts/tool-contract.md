# Contract: Tool pipeline and permission resolution

**Plan**: [../plan.md](../plan.md) | **Spec**: FR-030–FR-042, FR-035–FR-040, FR-061–FR-063 | **Source**: `internal/{tools,permissions,hooks}`

Translated from the upstream `contracts/tool-contract.md` (README §6). **Every tool call — builtin, skill-provided, remote (MCP),
a delegation, a board action, a retrieval — passes the same sixteen steps and the same ten layers. There is no second path.**

## 1. The Tool interface (no new ABI, ever)

```go
// internal/tools/types.go
type Tool interface {
    ID() ToolRef                                                  // {namespace}/{name}@{version}
    Descriptor() Descriptor                                       // ID, Description, InputSchema, EffectClass
    Taint() Taint                                                 // ReturnsUntrusted, ReadsPrivateData, MutatesExternal
    IsConcurrencySafe(input json.RawMessage) bool                 // PER INVOCATION, default false
    CheckPermissions(ctx, in json.RawMessage, rc RunContext) PermissionResult // Gate 2: deny | ask | defer — never allow
    ValidateInput(ctx, in json.RawMessage, rc RunContext) error
    Call(ctx, in json.RawMessage, rc RunContext) (Result, error)
}
type EffectClass string // read_only | mutating | external
type Taint struct{ ReturnsUntrusted, ReadsPrivateData, MutatesExternal bool } // DefaultTaint() = all TRUE (fail closed)
type Result struct{ Output json.RawMessage; IsError bool; Reason string; AwaitingChildSessionID *uuid.UUID } // last: delegate only
type RunContext struct{ TenantID, SessionID uuid.UUID; WorkspaceDir string; Sandbox SandboxExec } // nil Sandbox = unsandboxed fallback
```

- **Defaults fail closed**: serial unless proven concurrency-safe; assume writes; every taint leg `true` unless narrowed.
- **A tool declares; the platform decides.** `CheckPermissions` may only return deny/ask/defer — the chain treats an `allow` from
  Gate 2 as a bug, not an answer.
- **Skills, delegation, board and retrieval add no ABI**: `activate_skill`, `read_skill_file`, `delegate`, `read_board`,
  `claim_card`, `write_card`, `update_card_status` and `retrieve` are ordinary `Tool`s.

### Shipped catalog (resident)

`platform/file_read@v1` · `file_write@v1` · `file_search@v1` · `shell@v1` · `web_fetch@v1` · `web_crawl@v1` · `connector_fetch@v1` ·
`ask_clarification@v1` · `activate_skill@v1` · `read_skill_file@v1` · `delegate@v1` · `read_board@v1` · `claim_card@v1` ·
`write_card@v1` · `update_card_status@v1` · `retrieve@v1`. Remote tools are qualified `mcp/{server}/{tool}@{version}` and resolved
**dynamically per tenant and per user** (a `DynamicResolver` consulted at step 1 on a manifest miss).

Notable declarations: `web_crawl` declares **exactly** `web_fetch`'s taint (`ReturnsUntrusted`, `MutatesExternal`, not
`ReadsPrivateData`) — a table-driven test asserts field-for-field equality, because delegating a fetch to another service never
delegates the trust decision. `retrieve` declares `ReadsPrivateData`. `ask_clarification` forces Gate 2 to `ask` **regardless of
autonomy**, riding the existing approval suspend/resume machinery with no kernel change.

## 2. Identity, manifest, admission (FR-030–FR-032)

| Rule | Mechanism |
|---|---|
| One owner per namespace; collisions refused at **admission**, never by registration order | `identity.go` |
| The manifest pins the *resolvable* universe into `harness_digest`, including under deferred disclosure | `manifest.go` |
| Descriptor injection scan: `pending → clean \| flagged \| rejected`, fail closed | `admit.go` |
| Descriptor digest **re-verified at use** (step 2) — admission is not trusted forever | `dispatch.go` |
| `tool_loaded` events land in the **volatile** zone, never the byte-stable prefix | `kernel` + `manifest.go` |

## 3. The sixteen-step pipeline (FR-033)

`Pipeline.Execute(ctx, Invocation) ExecuteResult` — steps 1–11 in `execute.go`, 12–16 in `dispatch.go`. Each is individually unit-tested.

| # | Step | Short-circuits with |
|---|---|---|
| 1 | **Resolve** the qualified ref against the session's pinned manifest, falling through to the dynamic resolver on a miss | `unknown_tool` |
| 2 | **Digest re-verify** — the resolved descriptor must still match what the manifest pinned | typed error |
| 3 | **Admission gate** — cached verdict must be `clean` (a dynamic tool's tenant admission was enforced inside the resolver, fail closed) | `admission_<status>` |
| 4 | **Input validation** — `Tool.ValidateInput` | `invalid_input` |
| 5 | **Canonical digest (bind)** over `{tool_id, input}` | `digest_error` |
| 6 | **Gate 2** — the tool's own `CheckPermissions` | `gate2_error` |
| 7 | **PreToolUse hooks** — may DENY/ASK/DEFER, or rewrite input through a path allowlist | — |
| 8 | **Digest re-bind** if step 7 rewrote the input ("step 9a" re-verification) | — |
| 9 | **Permission chain** — the ten layers (§4), folding in steps 6 and 7 at their layers; taint transition computed here | — |
| 10 | **Decision gate: DENY** — typed, audited denial | `PermissionDenied` |
| 11 | **Decision gate: ASK** — typed, audited suspend request (carries `CanonicalDigest`, `EffectClass`, `AskKind`) | `AwaitingApproval` |
| 12 | **Concurrency-safety gate** — `IsConcurrencySafe` (a seam; no cross-worker lock until Phase 6) | — |
| 13 | **Call** — a panic is recovered into a typed error result; **write-ahead claim** opened first for every non-read-only effect (§6) | typed error |
| 14 | **PostToolUse hooks** — observe-only, tighten-only | — |
| 15 | **Result budgeting** — cap/paginate to ~25k tokens, spill the rest to the blob dir, return a preview + *"do not infer success from the preview"* | — |
| 16 | **Emit** the final `ExecuteResult` the kernel pairs to the `tool_use` | — |

`ExecuteApproved(…, approvedDigest)` re-runs steps 1–5 at resume and **compares the recomputed digest to the approved one**; a
mismatch yields `ApprovalMismatch` (a typed `approval_mismatch` event) — **never a silent re-request** (FR-056).

`ExecuteResult` also carries `AwaitingDelegation`/`ChildSessionID` (for `delegate`, resolved out-of-band by the return-time fold,
not a human) and `TaintChanged`/`TaintEngaged [3]bool`.

## 4. The ten-layer permission chain (FR-035)

One published **total order**. Every invocation walks it. `Decision ∈ {allow, ask, deny, defer}`; `Allow` appears **only** as the
chain's final `Resolution` — no layer 1–9 may return it (a layer that does is treated as a bug).

| # | Layer (`permissions.Layer`) | May return | Notes |
|---|---|---|---|
| 1 | `deny_rules` — tenant/tool/pattern | DENY (**final**) | glob matching (`glob.go`) |
| 2 | `pre_tool_use_hooks` | DENY (final) \| ASK \| DEFER | **never ALLOW** — a hook "allow" is coerced to DEFER |
| 3 | `autonomy` — pinned, ratchet | DENY \| ASK \| DEFER | a `read_only` *effect* always defers; for any other effect: `read_only` autonomy DENYs, `supervised` ASKs (`once`), `autonomous` defers; an unrecognized level DENYs |
| 4 | `gate1_tool_profile` — profile membership | DENY \| DEFER | versioned tenant config; **never ALLOW** |
| 5 | `gate2_capability` — tool metadata | DENY \| ASK \| DEFER | from step 6 |
| 6 | `gate3_safety` — per-invocation | DENY \| ASK \| DEFER | **ALWAYS EVALUATED** |
| 7 | `rule_of_two` — taint + declaration | ASK \| DEFER | **ALWAYS EVALUATED**; a third engaged leg ⇒ ASK, never DENY |
| 8 | `approval_policy` — per effect class | AUTO \| ASK(`once`\|`session`\|`multi_party`) | tenant policy can demand an ask upstream did not |
| 9 | `standing_scope` — batch / preauth | **SATISFIES** an ASK | **never suppresses** one, never manufactures an ALLOW |
| 10 | `fallback_allow` | ALLOW | otherwise |

**Two invariants make the order load-bearing, not decorative**: (a) a DENY at any layer is final — `Resolve` stops immediately and no
bypass mode exists; (b) layers 6 and 7 are unconditional — a remembered "yes" answers a question, it never grants permission to stop
asking it. Both are properties of `Resolve`'s loop structure, not of any one layer. **Skills sit *below* this table** as an
intersection pre-filter, never as a layer — a layer could resolve ALLOW, and "load this skill" must never widen anything.

**Autonomy** (`autonomy.go`): `read_only → supervised → autonomous` is pinned at run start. `Autonomy.Tighten(target)` is the only
mutator; **no widening function exists on any exported surface** (asserted by test). **Safety** (`safety/`): a deterministic rule pass
in-process, then a model leg with a bounded timeout and a circuit breaker, **failing closed to ASK**; the model leg is metered like any
model call. **Rule of Two** (`ruleoftwo.go`): `ResolveRuleOfTwo(state, taint) (LayerOutcome, TaintState)`; engagement is real whether
or not the human ultimately approves; `Rebaseline` clears **only** the untrusted-input leg.

## 5. Hook contract (FR-040)

```go
type Event string    // pre_tool_use | post_tool_use
type Kind  string    // command | http (SSRF-guarded) | prompt
type Config struct { Name; Event; Kind; Matcher string; If *Expr; Timeout; UpdatablePaths []string; … }
```

- `Matcher`: `"*"`, a bare namespace, a `namespace/*` glob, or an exact ref; `If` is a JSON-AST predicate (closed, like plan predicates).
- **Tighten-only.** A hook can DENY, ASK or DEFER; "allow" is normalised to DEFER before it reaches a caller (acceptance test in
  README §5). PostToolUse can flag or redact; it can never invert a failure into a success.
- **Bounded**: per-hook timeout (default **block**), a chain budget, a per-turn cap, a decision cache.
- **Rewrites**: only through `UpdatablePaths` (top-level field allowlist); a rewrite outside it — or any rewrite when the list is
  empty — is **refused, not silently dropped**; an accepted rewrite **re-binds the digest** (step 8).
- A `prompt` hook is a model call and is metered like any other.

## 6. Effect claims (FR-068)

Around `Tool.Call` for every non-read-only effect: `Claims.Open(tenant, session, toolID, digest) → (claimID, ClaimOutcome)` writes
`in_flight` **before the effect leaves the process**; a `completed` claim short-circuits a repeat; `Complete(…, failed, reason)` closes
it. On resume an `in_flight` claim is resolved by **probe or human** (`Control.ResolveClaim`), never by re-execution, never by silent
discard; `ErrUnresolvedClaims` blocks a resume that would otherwise skip one.

## 7. Egress and sandbox (FR-061, FR-062)

- `platform/web_fetch` and `platform/web_crawl` check the **target URL's host** against `NEXUS_WEB_FETCH_ALLOWLIST` (empty = none
  allowed). Connector and MCP endpoints are in the sandbox **deny set** — the in-sandbox broker is not shipped, so a bypass is never
  the interim state.
- `platform/shell` runs through `RunContext.Sandbox` when `NEXUS_SANDBOX` is `docker` or `opensandbox`; unset runs unsandboxed; a
  configured-but-unreachable backend **logs a warning and falls back** (never a hard failure). Docker limits: `--network none`,
  CPU/memory/PID/wall; `CapDrop ALL`, read-only root, `no-new-privileges`, user `65534:65534`; the per-session workspace stays writable.
