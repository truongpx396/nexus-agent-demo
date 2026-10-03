# Contract: Control-plane `v1`

**Plan**: [../plan.md](../plan.md) | **Spec**: FR-012, FR-110 | **Source**: `internal/controlplane/{port,local}.go` | **Closes**: production-readiness finding F15 (task 15.4)

The control-plane/data-plane split is a **Go interface**, not a network boundary. `Port` is what a genuinely separate control-plane process
would expose over RPC later; `LocalPort` is the one implementation this single binary needs, composing the real `internal/{cost,audit,
oversight,obs}` services. **One process today, two later** means replacing `LocalPort` with an RPC client behind this same interface —
a `cmd/nexusd` change, not a kernel change and not a `v1` shape change.

## Rules (enforced by `tests/contract/boundaries_test.go`)

- `internal/controlplane` MUST NOT import `internal/{sandbox,memory,provider}` — the data-plane-only packages.
- Every `v1` type is **plain data** (`uuid.UUID`, `string`, `[]byte`, `time.Time`, `json.RawMessage`) — never a richer domain type from the
  packages `Port`'s implementations wrap, so the wire shape does not move when an internal type does. Money crosses as a **decimal
  string** (`"0.05"`), never `cost.Money` and never a float.
- **Encryption stays on the data-plane side.** DEK minting and sealing (`internal/crypto`) are outside this boundary on purpose.
- **Each call is its own atomic operation.** There is no ambient shared transaction (it is impossible across a real RPC boundary). A caller
  sequencing several calls accepts that a failure between two leaves a *bounded, inert partial state* — an admitted-but-unresourced session
  simply accumulates nothing — never a security or double-spend hazard.
- **Routing is a data-plane decision** made *before* `AdmitRun`; the resolved model id and reason arrive as already-decided strings.

## Port (six calls)

```go
type Port interface {
    AdmitRun(ctx, AdmitRunV1) (AdmitRunResultV1, error)                      // session row + optional session-scoped budget
    ReserveBudget(ctx, ReserveBudgetV1) (ReserveBudgetResultV1, error)       // = cost.Gate.Reserve; fail closed on unpriced meter / exhausted ceiling
    ReportCost(ctx, ReportCostV1) error                                      // = cost.Gate.ReconcileUsage; ReservationID must be one THIS Port returned
    EmitAuditReceipt(ctx, EmitAuditReceiptV1) error                          // = audit.Chain.Append: one hash-chained receipt per durable event
    RequestApproval(ctx, RequestApprovalV1) (RequestApprovalResultV1, error) // = oversight.Approvals.Create
    AuthorizeContentAccess(ctx, AuthorizeContentAccessV1) (GrantV1, error)   // = obs.Grants.RequestGrant: audited, expiring
}
```

| Type | Fields |
|---|---|
| `AdmitRunV1` | `TenantID, UserID, SessionID uuid; SurfaceID, DataLabel, RouteModelID string; RouteReason map[string]string; Autonomy string; BudgetUSD string` (empty = no session ceiling) `; HarnessDigest []byte; Conversational bool` |
| `AdmitRunResultV1` | `Admitted bool; Reason string` — `Admitted=false` with a reason (bad autonomy, unparseable budget) is a **normal refusal**; a returned `error` means the durable write itself failed |
| `ReserveBudgetV1` | `TenantID, SessionID uuid; ModelID, Purpose string` |
| `ReserveBudgetResultV1` | `ReservationID uuid; Decision string` (`allow\|refuse_ceiling\|degrade\|skip`)`; Reason, ReservedUSD string` |
| `ReportCostV1` | `ReservationID uuid; InputUncached, InputCacheRead, InputCacheWrite, OutputTokens int; Reported bool` — `Reported=false` is the **UNREPORTED** case: charged at the full reserved worst case, never assumed free |
| `EmitAuditReceiptV1` | `TenantID, SessionID uuid; Seq int64; EventID uuid; EventType string; PayloadDigest []byte` |
| `RequestApprovalV1` | `TenantID, SessionID, ToolUseEventID uuid; ToolID, AskKind string; CanonicalDigest []byte; EffectClass string; Input json.RawMessage` — `Input` is the tool_use's **plaintext original** (an approver needs to see it, not a bare digest), never the sealed payload |
| `RequestApprovalResultV1` | `ApprovalID uuid; ExpiresAt time.Time` |
| `AuthorizeContentAccessV1` | `TenantID, SessionID, GranteeID uuid; Reason string; TTL time.Duration` |
| `GrantV1` | `GrantID uuid; ExpiresAt time.Time` |

## Versioning

`v1` suffixes every type. A breaking change adds `…V2` types and a `PortV2` alongside — it never edits a `V1` shape. The REST surface calls
`AdmitRun` from `handleCreateRun` after resolving routing and computing the harness digest; the kernel never imports this package.

## Known limitation

`AdmitRunV1` carries `HarnessDigest []byte` computed by the caller. Because the REST path never sets the safety-policy and approval-policy
version inputs (spec Known deviation 5), the digest admitted here does not reflect them.
