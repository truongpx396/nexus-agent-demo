# Contract: Event taxonomy and envelope

**Plan**: [../plan.md](../plan.md) | **Spec**: FR-004–FR-006, FR-017, FR-018 | **Source**: `internal/store/{event,upcast,append,projection}.go`, `migrations/0003_events.sql`

The event log is the **single source of truth** and a **versioned contract**. It "must stay complete enough to reconstruct any run — including who
authorized what, on what basis, and when — from the log alone." 60 types, `CurrentSchemaVersion = 2`.

## Envelope (every event)

| Field | Meaning |
|---|---|
| `event_id` | uuid |
| `session_id`, `tenant_id` | scope; `tenant_id` first (RLS) |
| `seq` | strictly sequential per session; `UNIQUE (session_id, seq)` |
| `schema_version` | the envelope version the payload was written under (now **2**) |
| `type` | one of the 60 below |
| `payload` | **ciphertext** under `key_id` (per-tenant DEK) |
| `payload_digest` | digest over the **plaintext**; survives crypto-shredding; the only thing the audit chain commits to |
| `key_id` | destroying this key **is** erasure |
| `actor` | `model` \| `tool` \| `user` \| `system` |
| `tool_id` | qualified `{ns}/{name}@{ver}` when relevant |
| `pair_ref` | on a `tool_result`: the `event_id` of its `tool_use` — **the paired-result invariant** |
| `model_id` | stamped on model-produced events for audit |
| `trace_id`, `span_id` | bidirectional join to telemetry |

**Immutability**: trigger rejects `UPDATE`/`DELETE` for every role; `REVOKE` refuses it earlier for `nexus_app`.
**Receipts**: one hash-chained receipt per event, appended in the **same transaction** (see [signer-protocol.md](signer-protocol.md)).

## Upcasting (FR-005)

`upcastRegistry[type][fromVersion]` transforms a payload from `fromVersion` to `fromVersion+1`, registered in one place so the whole path for a
type is readable at once. Today there is one real entry: `content` v1 `{"text": …}` → v2 `{"body": …}` (renamed to match every other
content-bearing event). Replaying an old run applies the chain of upcasters; a test proves a v0→v1-style upcast. *Without this, "replay a run from
years ago" is a claim, not a property.*

## Taxonomy (60)

| Group | Types |
|---|---|
| **Model output** (3) | `thought` (opaque reasoning; **body never shown**, even to the audience) · `content` · `tool_use` |
| **Tool** (4) | `tool_result` (carries `pair_ref`; may be **synthetic**) · `tool_receipt_ref` · `effect_claimed` · `effect_claim_resolved` |
| **Context** (3) | `condensation` (model-facing only) · `context_pruned` (a *view*; never mutates a logged event) · `checkpoint` |
| **Human — push** (2) | `user_message` · `awaiting_input` (a conversational pause; **distinct** from `input_requested`) |
| **Human — pull** (4) | `input_requested` · `input_answered` · `input_expired` · `input_invalidated` — zero authorization value |
| **Approval** (11) | `approval_requested` · `approval_notified` · `approval_reminded` · `approval_escalated` · `approval_granted` · `approval_granted_modified` · `approval_denied` · `approval_expired` · `approval_invalidated` · `approval_resolution_refused` · `approval_mismatch` |
| **Cost** (1) | `budget_decision` — one per gate resolution, including `skip` |
| **Catalog** (1) | `tool_loaded` — lands in the **volatile** zone |
| **Memory** (1) | `memory_loaded` — one per session start |
| **Skills** (2) | `skill_activated` · `skill_capability_ignored` (the only skill-specific types; no per-action events) |
| **Delivery** (4) | `delivery_enqueued` · `delivery_delivered` · `delivery_failed` · `delivery_suppressed` |
| **Safety** (2) | `taint_transition` (cumulative `Engaged`, not a delta) · `sanitization_boundary` |
| **Delegation** (5) | `delegation_requested` · `delegation_target_selected` · `delegation_refused` · `delegation_returned` · `delegation_reaped` |
| **Orchestration** (5) | `plan_started` · `plan_step_entered` · `plan_transition` · `plan_step_exited` · `plan_completed` |
| **Content access** (3) | `content_access_granted` · `content_accessed` · `content_access_refused` — a receipt on grant **and every read** |
| **Lifecycle** (5) | `error` · `stuck_suspected` (non-terminal) · `forked` · `terminal` · `erasure` |
| **Control** (1) | `autonomy_tightened` |
| **Teams** (3) | `team_created` · `team_ended` · `board_card_flagged` (board reads/claims/writes use the ordinary `tool_use`/`tool_result` pair) |

> Delivery status `failed_permanent` is a **row** state in `deliveries`, not an event type; the event vocabulary has no `delivery_failed_permanent`.

## Producers and invariants

- **Paired results** (FR-018): every `tool_use` has exactly one `tool_result` with `pair_ref` set before the next model call; on cancel, error, crash or
  hygiene backfill the result is **synthetic** (a real row with a `Reason`). Property-tested over generated histories
  (`tests/property/paired_result_test.go`) — it is a *total* invariant, so examples are not enough.
- **`terminal`** (FR-017): exactly one per run, its sealed payload `{reason, detail}`; reason ∈ the ten terminal reasons. A *conversational* run
  emits `awaiting_input` instead and no `terminal` until cancelled or `idle_timeout`.
- **`taint_transition`** is appended right after `tool_result` and **before** any suspend/terminate the call's outcome triggers; three producers —
  an ordinary tool call's Rule-of-Two resolution, delegation's fold-at-return, and a board-card read.
- **Projections** are rebuilt by replay: `ReplayProjection` (structural, no decrypt — status only) and `ReplayFullProjection` (adds one selective
  decrypt of the last `terminal` for the exact reason). Status moves: `user_message|tool_use|tool_result|content|thought` → `running` (unless
  `suspended`); `approval_requested|input_requested` → `suspended`; `awaiting_input` → `awaiting_input`; any approval resolution or input resolution →
  `running`; `terminal` → ended.
- **`erasure`** is itself an event: crypto-shredding appends it, destroys the key, and leaves the sequence, digests and chain intact.
- **`approval_mismatch`** and **`approval_resolution_refused`** are typed outcomes, never silent retries.
- **Writers** (a grep of non-test callers of `store.Append` at `8e02491`; an *observation*, not something a test enforces): `kernel/`,
  `internal/{oversight,delegate,teams,plan,runctl,crypto,obs}` and `internal/surfaces` (the delivery outbox). Every one goes through
  `store.Append`, which is what makes the append-only trigger, the receipt hook and the sealing function the single choke point.
