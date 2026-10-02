# Contract: Kernel ABI

**Plan**: [../plan.md](../plan.md) | **Spec**: FR-015–FR-029, FR-064–FR-082 | **Source**: `kernel/`, `internal/{provider,cost,store,oversight,runctl,memory,skills,surfaces}`

The kernel ABI is the set of seams the loop depends on. It is translated from the upstream design's
`contracts/kernel-abi.md` (README §6) into Go **as shipped** — signatures below are abridged from the code at `8e02491`,
and where they differ from README §3's illustrative interfaces, **the code wins** (differences are called out).

**Dependency rule** (`tests/contract/boundaries_test.go`): `kernel/` may import `internal/{provider,tools,promptctx,store,cost,
reliability,obs}` and nothing else. It must not import `internal/surfaces` or `internal/controlplane`. Surfaces must not import
`kernel/`; the only place a `kernel.RunState`/`RunConfig` is built is `cmd/nexusd/runner.go`'s `kernelRunStarter`.

**Nil-valid optional hooks.** Every optional `Kernel` field (`Receipts`, `OnSuspend`, `OnDelegate`, `Stuck`, `Tracer`, `OnChunk`,
`PrunePolicy`, `CondenseThresholdBytes`) means *"that control isn't wired"* when nil/zero. This is how minimal kernels are built in
tests. **New hooks MUST follow this convention.**

## 1. Kernel

```go
// kernel/loop.go — the ONLY place agent control flow lives.
type Kernel struct {
    Provider provider.Provider
    Tools    ToolExecutor
    Budget   BudgetGate
    Store    *store.Store
    // optional, nil-valid:
    Receipts ReceiptFunc; OnSuspend OnSuspend; OnDelegate OnDelegate
    Stuck *reliability.Registry; PrunePolicy promptctx.PrunePolicy
    CondenseThresholdBytes int
    Tracer obs.Tracer; TraceContent bool; OnChunk OnChunk
}

func (k *Kernel) Run(ctx, *RunState, RunConfig) iter.Seq2[store.Event, error]
func (k *Kernel) Seed(ctx, *RunState, RunConfig) iter.Seq2[store.Event, error]              // opening event, durably appended BEFORE queueing
func (k *Kernel) Resume(ctx, *RunState, RunConfig, PendingResolution) iter.Seq2[...]         // one approval/input-suspended tool_use
func (k *Kernel) ResumeDelegation(ctx, *RunState, RunConfig, DelegationResolution) iter.Seq2[...]
func (k *Kernel) Continue(ctx, *RunState, RunConfig) iter.Seq2[...]
func (k *Kernel) ResumeConversation(ctx, *RunState, RunConfig, input string) iter.Seq2[...]
```

**Loop order** (FR-015–FR-019): hygiene → reserve → stream → classify → dispatch → pair → terminate. Dispatch is on the typed
classification (`TOOL_CALLS | CONTENT | EMPTY`), never on text. Every exit is exactly one `TerminalReason`; every switch over
`TerminalReason` and over response classification is exhaustive (`exhaustive` runs with `default-signifies-exhaustive: false`,
so a `default:` does **not** satisfy it).

**Differences from the README sketch**: `Kernel` is a *struct* with six entry points, not a one-method interface; `Seed` was
extracted from `Run` so a fresh run's opening event is durable before it is queued; `Resume*`/`Continue` exist because
suspend/resume is a first-class state, not an exception.

### RunConfig (what a caller supplies per run)

`System`, `Catalog []provider.ToolSchema`, `ModelID` (the router's decision), `MaxTurns` (a backstop only), `Input` (opening user
message; empty on resume), `AutonomyLevel` (`read_only|supervised|autonomous`, default `supervised`), `LoadedTools` (qualified refs →
one `tool_loaded` event each, in the **volatile** zone), `CondenserModelID`, `MemorySources` (→ one `memory_loaded` event),
`Conversational` (a plain reply pauses to `awaiting_input` instead of terminating).

### Terminal reasons

`completed` · `max_turns_exceeded` · `cost_exhausted` · `aborted` · `stuck_terminated` · `permission_denied` · `context_overflow` ·
`error` · `refused` · `idle_timeout`. Each has a named producer (`kernel/terminal.go`): the loop (`completed`, `max_turns_exceeded`,
`context_overflow`, `error`, `refused`), the cost gate (`cost_exhausted`), run control (`aborted` — its **sole** producer), the
stuck detector (`stuck_terminated`), the permission chain (`permission_denied`), and the idle sweep (`idle_timeout`).

## 2. Provider (FR-021–FR-025)

```go
// internal/provider/provider.go
type Provider interface {
    Stream(ctx, p Prompt, tools []ToolSchema, rc RunContext) (Stream, error)
}
type Stream interface { Next(ctx) (chunk Chunk, ok bool, err error) }
type Embedder interface { Embed(ctx, texts []string, rc RunContext) ([]Embedding, EmbedUsage, error) }

type ChunkKind string // content | reasoning | tool_use | usage | done
type DoneReason string // stop | max_output | error | refusal
type Usage struct { InputUncached, InputCacheRead, InputCacheWrite, OutputTokens int }
type Chunk struct {
    Kind ChunkKind; Text string; Opaque []byte            // reasoning: round-tripped, never shown
    ToolUseID, ToolName string; Input json.RawMessage     // tool_use
    Usage Usage; Done DoneReason; RefusalCategory string   // usage / done
}
type Message struct { Role; Blocks []ContentBlock }        // typed blocks carry tool_use ids to the wire (F5)
```

- **Native tool calling only** — no parsing tools out of free-form text.
- **Typed errors**: `*ThrottleError`, `*ContextOverflowError`. Context overflow is **never** failed over.
- **Failover** (`failover.go`): retryable / permanent / context-overflow; the stream is **committed after its first chunk**.
- **Adapters**: `anthropic` (HTTP, adaptive thinking, ephemeral cache breakpoint closing the stable zone), `litellm`, and
  `fake` (YAML-scripted: truncation, stall, malformed stream, throttle, failover; **mandatory** for correctness tests).
- `DoneRefusal` maps to terminal `refused` and carries `RefusalCategory`; it is never folded into `DoneStop`.
- **Routing** (`router.go`): `Route(dataLabel, difficulty)` is deterministic; the decision and reason are persisted on the session.

## 3. Tool executor and budget gate

```go
// kernel/types.go
type ToolExecutor interface { Execute(ctx, ToolUseRequest, ExecContext) ToolResult }
type ApprovedExecutor interface { ExecuteApproved(ctx, ToolUseRequest, approvedDigest []byte, ExecContext) ToolResult } // optional
type TaintSeeder interface { SeedTaint(sessionID uuid.UUID, autonomyLevel string, engaged [3]bool) }                    // optional

type BudgetGate interface {                       // consulted before EVERY provider call
    Reserve(ctx, cost.ReserveRequest) (cost.Reservation, error)
    Reconcile(ctx, cost.Reservation, provider.Usage, reported bool) error
}
```

`ToolResult` carries `IsError`, `Synthetic`, `Reason`, `AwaitingApproval`/`AskKind`, `AwaitingDelegation`/`ChildSessionID`, and
`TaintChanged`/`TaintEngaged [3]bool` (the loop appends a `taint_transition` event whenever `TaintChanged`, right after `tool_result`).
The full pipeline contract is in [tool-contract.md](tool-contract.md). **`Reconcile` accepts `reported=false`** (the UNREPORTED case):
the call is charged at the full reserved worst case and flagged (FR-049).

*Differences from the README sketch*: the gate is `Reserve`/`Reconcile` (no `Record`), and `Reserve` returns a `Reservation`
whose `Decision` is one of `allow | refuse_ceiling | degrade | skip`. Every `Provider.Stream` caller — compaction, safety leg,
prompt hooks, judge, titles, embeddings — must pass through it (FR-050); `tests/contract/{cost,embedding}_metering_test.go`
enforce this by AST check.

## 4. Persistence (FR-004–FR-006, FR-065–FR-068)

```go
// internal/store — one log, three non-interchangeable artifacts. Concrete package functions over a pgx.Tx, not an interface.
func Append(ctx, tx pgx.Tx, e Event) (Event, error)               // append-only; returns the event with its assigned seq; receipt written in the SAME tx
func ListEvents(ctx, tx pgx.Tx, sessionID uuid.UUID) ([]Event, error)
func (s *Store) InTenantTx(ctx, tenantID uuid.UUID, fn func(ctx, tx pgx.Tx) error) error // the ONLY tenant-scoping call:
                                                                   //   set_config('app.tenant_id', $1, true)
func ReplayProjection(history []Event) Projection                 // structural, no decrypt
func ReplayFullProjection(history []Event, decrypt TerminalDecryptFunc) (Projection, error)
// Checkpoint / Snapshot / Claim …                                 // see data-model.md §5
```

`SealFunc` (seal one payload → `sealed, plaintextDigest, keyID`) and `ReceiptFunc` (extend the audit chain inside the append
transaction) are declared **in `kernel/`**, not imported, so the loop and its property test need no crypto or database.

## 5. Oversight (FR-055–FR-059)

```go
// internal/oversight
Approvals.Create(ctx, CreateApprovalRequest) (Approval, error)
Approvals.Grant(ctx, tenant, id, decidedBy) / GrantModified(…, modifiedInput) / Deny(…, reason) (Approval, error)
Approvals.ListPending / Get / Invalidate(ctx, tenant, session, InvalidationReason)
Inputs.RequestInput / Answer / Expire / Invalidate                  // zero authorization value
Resumer.Grant / GrantModified / Deny(…) iter.Seq2[store.Event, error] // resumes the ONE suspended tool_use
```

Approval and input share **suspension and nothing else** (distinct lifecycles, distinct terminal behavior). Suspension is durable
(checkpoint + evict) at **zero token cost**; the suspended interval is excluded from latency measures.

## 6. Run control (FR-071, FR-072)

```go
// internal/runctl — Fork/Cancel/TightenAutonomy/Steer stay synchronous (none drives the turn loop)
Control.Steer(ctx, tenant, session, input) (store.Event, error)          // drained at a turn boundary; into a suspended run ⇒ invalidates its approval
Control.Cancel(ctx, tenant, session, reason) error                       // sole producer of `aborted`
Control.Resume(ctx, tenant, session) iter.Seq2[store.Event, error]
Control.TightenAutonomy(ctx, tenant, session, target) error              // ratchet; no widening function exists
Control.Replay(ctx, tenant, session) (ReplayResult, error)               // PURE: no model call, no tool, no append
Control.Fork(ctx, tenant, parent, atSeq, ForkOverrides) (ForkResult, error) // effects disabled; reports digest divergence
Control.ResumeConversation(ctx, tenant, session, input) (store.Event, error)
Control.EndIdleConversation(ctx, tenant, session) error                  // → idle_timeout
Control.ResolveClaim(ctx, tenant, session, claimID, status, reason) (store.Claim, error) // probe or human; never re-execution
```

## 7. Memory and skills (FR-073–FR-078)

```go
// internal/memory — file-first; writes take effect NEXT session
Store.Load / LoadForSession(ctx, …, tenantID) (Snapshot, error)
Store.Write(tenantID, name, content) error
Store.Consolidate(tenantID, name, reserve func() bool, condenser func(string) (string, error)) error // ordered, metered, degrade-capable

// internal/skills — signed, content-addressed bundles; activation is an ordinary tool (platform/activate_skill)
Catalog.Resolve(skillID) (SkillBundle, bool)
Catalog.ReadFile(skillID, path) ([]byte, error)   // tier 3, reference content only; a script is NEVER fetched this way
```

`declared_tool_ids` **intersects** the currently resolved catalog at activation (never union); an absent entry emits
`skill_capability_ignored`. A bundled script registers as a real tool through the ordinary gates (`ScriptTool`) or the bundle is refused.

## 8. Surfaces (FR-079–FR-082)

```go
// internal/surfaces/capability
type Descriptor struct {
    SurfaceID string; PrincipalKind PrincipalKind          // user | scheduler
    CanRenderApprovalContext, SupportsStepUp, SupportsStructuredInput, SupportsStreaming bool
}
type Principal struct { Kind PrincipalKind; TenantID, UserID uuid.UUID }  // resolved PER TURN
```

Each surface declares a descriptor and passes the conformance suite; approval routing filters on capability. Authority is the
**turn-submitting** principal. Outbound delivery goes through `surfaces.Outbox` (event appended **before** the send; idempotent on
`(session, seq, surface, recipient)`).

## 9. Telemetry (FR-009, FR-138)

```go
// internal/obs
type Exporter interface { Emit(name string, attrs Attrs) error }   // stdout, OTLP, native Langfuse; MultiExporter fans out
type Tracer interface { StartSpan(ctx, name, kind ObservationType, attrs Attrs) (context.Context, Span); Detach/Attach … }
Span.SetContent(input, output string)   // ⚠ NOT filtered by the allowlist — see plan.md Complexity Tracking / spec Known deviation 1
```

Attributes pass a **deny-by-default allowlist** (`allowlist.go`) enforced at the exporter. `Span.SetContent` is the one channel that
does not, gated by `Kernel.TraceContent` (`NEXUS_TRACE_CONTENT`, default **false**); it contradicts the constitution's "no flag may
admit content" and is recorded as a deviation, not endorsed by this contract.

## 10. Delegation and sandbox seams

- **Delegation** (`internal/delegate`): a *tool invocation* through the same pipeline (`platform/delegate`), not a side channel.
  `Spawn(ctx, SpawnRequest) (childSessionID, error)`; bounds `MaxDepth=1`, `MaxConcurrent=3`, `MaxPerRun=16`; `EnvelopeBudgetGate`
  draws a fan-out's children from one pre-reserved envelope.
- **Sandbox** (`internal/tools.SandboxExec`): `Exec(ctx, cmd) (output string, exitCode int, breach string, err error)`. Declared
  structurally so `internal/sandbox.SessionSandbox` (Docker) and `internal/sandbox/opensandbox.go` satisfy it with no shared
  declaration — the decoupling idiom the interface's own comment names.
