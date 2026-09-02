# Go-live checklist

README task 10.13 / docs/constitution.md's own **Go-live gate**: *"No
production launch without the go-live checklist green — attributable audit
log, vaulted per-tenant secrets, sandboxing + human approval for high-impact
actions, at least one leg of the lethal trifecta broken per risky flow,
per-task/per-tenant cost ceilings, failure classification + resume + stuck
detection, evals green in CI, cache-read >90% steady-state, documented data
residency/retention/no-train, and a rehearsed behavioral-incident runbook."*

Each item below is either **AUTOMATED** (checked by `nexusctl go-live` /
`make go-live` against a live deployment — the "script" this task names)
or **MANUAL** (a design/process/documentation review this codebase cannot
verify by querying its own database, and does not pretend to).

| # | Item | How it's checked | Kind |
|---|---|---|---|
| 1 | Attributable audit log | `nexusd verify-chain`'s own pass, re-run per tenant: every receipt hash-chains, every anchor covers a contiguous range, no gap in sequence | AUTOMATED |
| 2 | Vaulted per-tenant secrets | The process's configured KEK is reachable and `signerd` answers over its socket (sign-only — `nexusd` is never shown the key, README task 5.1) | AUTOMATED (reachability only — rotation policy and vault backend choice are MANUAL) |
| 3 | Sandboxing + human approval for high-impact actions | Structural: this is a property of the shipped code (internal/sandbox, internal/oversight), not of runtime data — confirmed once at code-review time, not per deployment | MANUAL |
| 4 | At least one leg of the lethal trifecta broken per risky flow | A design review per flow (untrusted input × private data access × external communication, docs/constitution.md Principle V) — no query over an event log can substitute for this judgment | MANUAL |
| 5 | Per-task/per-tenant cost ceilings | At least one row in `budgets` for every tenant (`scope='tenant'`), and no tenant's `cost_ceiling_breach_rate` (golden-signal dashboard, task 10.12) sitting at 0 in a way that suggests the ceiling was never actually reserved against | AUTOMATED |
| 6 | Failure classification + resume + stuck detection | `unresolved_in_flight_claims` (golden-signal dashboard) is 0 or explainable — a claim stuck `in_flight` past its staleness window is exactly the failure mode task 6.6 exists to make impossible | AUTOMATED |
| 7 | Evals green in CI | `evals/testdata/baseline.json` exists, loads, and its `Environment` digest matches this build's own eval-gate run — i.e., the CI `eval-gate` job (`.github/workflows/ci.yml`) that produced it is the SAME gate this deployment shipped behind, not a stale or hand-edited file | AUTOMATED |
| 8 | Cache-read rate >90% steady-state | `cache_read_rate` (golden-signal dashboard, from `cost_records`) across the tenant's own recent traffic | AUTOMATED (reports the measured rate; "steady-state" judgment — has traffic settled into its normal pattern yet — is MANUAL) |
| 9 | Documented data residency/retention/no-train | A written policy artifact outside this codebase | MANUAL |
| 10 | Rehearsed behavioral-incident runbook | A conducted rehearsal, recorded outside this codebase | MANUAL |

## Running it

```
make go-live TENANT=acme     # or: ./bin/nexusd go-live
```

Exits non-zero if any AUTOMATED item fails. Prints every MANUAL item as a
named reminder — never as a silent pass — because an unchecked box that
prints green is worse than no checklist at all.

See `internal/obs/dashboard.go` (the golden-signal queries) and
`cmd/nexusd`'s `runGoLive` for the implementation of every AUTOMATED item.
