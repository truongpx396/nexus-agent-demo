# Contract: Orchestration plane, delegation and teams

**Plan**: [../plan.md](../plan.md) | **Spec**: FR-083–FR-092, FR-111–FR-116 | **Source**: `internal/{plan,delegate,teams}`

Translated from the upstream `contracts/orchestration-plane.md` (README §6): plan schema, lifecycle gates, and the **zero-token routing
invariant**. Delegation and teams are added here because they obey the same discipline: *scope descends, taint ascends, bounds fail closed,
cost is reserved as an envelope up front, and nothing is a new tool ABI.*

## 1. Plan schema (FR-083)

```go
// internal/plan/schema.go
type Plan struct {
    PlanID, TenantID uuid.UUID; Version int; Name string
    StartStep    string
    Steps        []Step
    AllowedTools []string      // the plan's own declared tool universe; every fan-out scope_grant must be a SUBSET of it
    CostEnvelope CostEnvelope  // {Ceiling string} decimal via cost.ParseDecimal; empty = unenforced
}
type Step struct {
    ID string; Kind StepKind
    Agent *AgentStepConfig; DelegateFanout *DelegateFanoutConfig; ApprovalGate *ApprovalGateConfig
    Preauth *PreauthConfig; InputRequest *InputRequestConfig; Condition *ConditionConfig; Loop *LoopConfig  // exactly ONE set, matching Kind
    Transitions []Transition                                    // {To string; When *Predicate}; nil When = unconditional, must be listed LAST
}
```

| `StepKind` | Config | Meaning |
|---|---|---|
| `agent` | `{Input, OutputVar}` | one model-driven step |
| `delegate_fanout` | `{AgentID, Task, ScopeGrant[], ChildCount, PerChildCeiling, CompletionVar}` | N children under one pre-reserved envelope |
| `approval_gate` | `{Question, ResultVar}` | a human decision, via `internal/oversight.Approvals` |
| `preauth` | `{Entries: [{ToolID, Digest}]}` | one human decision over an **enumerated, digest-bound** set |
| `input_request` | `{Question, Schema, ResultVar}` | an agent→human pull; **zero authorization value** |
| `condition` | `{}` | pure routing on the predicate |
| `loop` | `{MaxIterations, CounterVar}` | the **only** step a cycle may pass through; `MaxIterations` must be positive |

## 2. Predicates are a closed AST (FR-084, SC-023)

```go
type Predicate struct {
    Op     Op          // eq | ne | lt | gt | and | or | in
    Field  string      // dot-free key into the plan run's flat variable Context
    Value  Value; Values []Value
    And, Or []Predicate
}
type Value struct { Kind ValueKind /* string | number | bool */; Str string; Num float64; Bool bool }
```

- **No** string evaluation, I/O, model call or unbounded loop. `Eval` reads a `Context` and writes nothing — it cannot have a side
  effect. That closure is what turns "routing costs zero tokens" from a promise into a **property of the type**.
- Typed operands: comparisons across kinds are never silently coerced (`eq` across kinds is `false`; `lt`/`gt` on a non-number is a
  *validation-time* error). Nesting is bounded by `maxPredicateDepth`.
- **Zero-token test** (SC-023): against the fake provider, assert no `Provider.Stream` call occurs while evaluating a transition.

## 3. Validation (FR-085) — `plan.Validate(Plan) error`, pure (no I/O, no tenant lookup)

1. `start_step` present and declared; ≥ 1 step; step ids non-empty and unique.
2. Step shape: exactly one kind-specific config set and matching `Kind`; `delegate_fanout.child_count > 0`, `scope_grant` non-empty,
   `per_child_ceiling` parses; approval/input steps have a non-empty question; loops have positive `max_iterations`.
3. Transitions target declared steps; the unconditional transition is last; predicate depth ≤ max.
4. **Reachability**: every step reachable from `start_step`; cycles only through `loop` steps.
5. **Cost envelope**: `ceiling` parses; a plan with any `delegate_fanout` **must** declare one.
6. **Scope subset**: every `delegate_fanout.scope_grant ⊆ AllowedTools`.
7. **Oversight completeness**: holds **structurally** — the schema has no field capable of suppressing an approval outside a bounded
   preauth enumeration — and is checked explicitly: every `preauth` entry must be a fully bound `{tool_id, digest}` pair, never empty
   or a wildcard. (It is *not* a lookup against a tenant's live approval policy; `Validate` is pure by design.)

## 4. Lifecycle (FR-086)

```
draft ──ValidatePlan──▶ validated ──RunEvalGate──▶ eval_passed ──SignOff(by)──▶ signed_off ──Enable(agentVersion, routeModelID)──▶ enabled ──Retire──▶ retired
```

`Lifecycle.{Create, NewVersion, Get, ValidatePlan, RunEvalGate, SignOff, Enable, Retire}`; the eval gate is an injected `EvalGate`
function, so the plan package does not import `evals/`. `Create` inserts version 1 as `draft`; `NewVersion` inserts the next version.
`Enable` **pins `agent_version` and `route_model_id` into the spec**; once `enabled`, every transition except `Retire` is refused —
**enabled versions are immutable**, and in-flight runs finish on the version they started on. `PRIMARY KEY (plan_id, version)`.

## 5. Execution (FR-087)

The **platform** evaluates transitions; the model never routes. A step boundary is a **checkpoint**. Events: `plan_started`,
`plan_step_entered`, `plan_transition` (names the predicate that fired), `plan_step_exited`, `plan_completed`. A plan run is driven by
its own session; its `CostEnvelope` becomes a session-scoped ceiling on it. Replay (`replay.go`) and rehydration (`rehydrate.go`) rebuild
plan position from these events. Same plan + same input ⇒ same branch (SC-024).

## 6. Delegation (FR-089–FR-092)

`platform/delegate@v1` is an **ordinary `Tool`** — same sixteen steps, same ten layers, same receipts. Input:

```json
{ "agent_id": "…", "task": "…", "scope_grant": ["platform/web_crawl@v1", "…"], "return_schema": { } }
```

| Rule | How |
|---|---|
| **Scope descends** | `CheckPermissions` re-derives `scope_grant` as a **provable subset of the parent's own resolved scope**; it is never trusted from the input, and there is no widening parameter |
| **Taint ascends** | a child's `taint_state` *starts as a copy of the parent's*; on return the parent folds in the child's **own event-derived** taint, **never a claim in the return payload** — a child cannot self-report "clean" (SC-021) |
| **Tool taint** | `Taint()` defaults all three legs TRUE, so autonomy and the Rule of Two gate a delegation **in addition to** the bounds |
| **Bounds fail closed** | `MaxDepth = 1`, `MaxConcurrent = 3` (open per root), `MaxPerRun = 16` (ever, per root); a breach ⇒ `bound_exceeded`, **non-retryable** (SC-022) |
| **Suspension** | the **same durable-suspend** primitive approvals use — checkpoint + evict; resume on `delegation_returned` / `delegation_reaped`; zero tokens while waiting |
| **Fan-out cost** | reserved as **one envelope before the first child** (`fanout_envelopes`); children draw from it via `EnvelopeBudgetGate`; per-child reservation against the tenant counter is **prohibited**. A single ad hoc `delegate` call is *not* a fan-out and reserves normally |
| **Reaping** | on parent terminal/cancel/ceiling; return-schema validation + acceptance criterion before folding a result in |

Events: `delegation_requested`, `delegation_target_selected`, `delegation_refused`, `delegation_returned`, `delegation_reaped`. States:
`pending → returned | reaped | bound_exceeded`. Two named conventions ship over the **unmodified** tool: `web-researcher`
(`scope_grant`: `web_crawl`, `web_fetch`, `activate_skill`) and `sandbox-runner` (`shell`, `activate_skill`). They are *documented
`agent_id`/`scope_grant` pairs*, not a per-agent system-prompt table — a per-agent prompt fork would move `harness_digest` and break
cache stability (README, Phase 16 task 16.8).

## 7. Peer teams (FR-111–FR-116)

```go
// internal/teams.Service
CreateTeam(ctx, CreateTeamRequest) (teamID, error)            // roster FIXED here; envelope reserved ONCE here
ReadBoard(ctx, tenant, team, readerSession) ([]Card, error)    // folds each card's taint into the reader's own taint
ClaimCard(ctx, tenant, team, card, session) (Card, claimed bool, error)   // SELECT … FOR UPDATE SKIP LOCKED — exactly one winner
WriteCard(ctx, WriteCardRequest) (Card, error)                 // injection-scans the body before status → clean
UpdateCardStatus(ctx, tenant, team, card, session, CardStatus) (Card, ok bool, error)
OnMemberTerminal / SweepBackstop(ctx, tenant, backstop)        // completion + wall-clock backstop; reaps every still-active member
```

- Members are **ordinary sessions** (`delegation_role = 'team_member'`, `team_id` set), reusing the session-key serial lock — no new
  locking primitive for the loop. A member is a **leaf**: it cannot `delegate` or create a team (preserves depth ≤ 1).
- Four tools — `read_board`, `claim_card`, `write_card`, `update_card_status` — are ordinary `Tool`s with all-TRUE taint.
- **Read-time taint fold** is the one genuinely new mechanism: reading a card folds its `taint_state` into the reader's — the same shape
  as delegation's return-time fold, triggered by a read. It closes the laundering path a shared board would otherwise reopen (SC-029).
- A `flagged` card is never surfaced to another member's context (fail closed).
- Lifecycle: `active → completed | aborted | ceiling_exhausted`. Completes when no card is `open`/`claimed` **and** every member is
  terminal; ends on envelope exhaustion (`cost_exhausted`) or the wall-clock backstop; ending reaps every still-active member.
- Events: `team_created`, `team_ended`, `board_card_flagged`.
