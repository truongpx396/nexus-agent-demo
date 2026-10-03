# Quickstart: validating the Nexus Agent Platform

**Plan**: [plan.md](plan.md) | **Spec**: [spec.md](spec.md) | **Contracts**: [contracts/](contracts/) | **Data model**: [data-model.md](data-model.md)

Runnable scenarios that prove each user story end to end. They are the demo/acceptance commands from `docs/build-phases.md`, reconciled with the
Make targets and tests that exist at `8e02491`. **Each scenario states honestly what is covered by an automated test and what is manual-only.** The
upstream design had a nine-scenario `quickstart.md` (README §6); this one is organised by *this* spec's stories instead.

Legend — **Auto**: a named test asserts it · **Manual**: you run it and look · **No check**: neither exists (a Phase 19 task).

## 0. Prerequisites and bring-up

- Go 1.25 (`go.mod` pins 1.25.14), Docker running, Node for `web/`. `jq` and `goimports` if you use the Claude Code hooks.
- **Ports**: Postgres `5433` (direct — admin/migrations only), PgBouncer `6432` (runtime), Redis `6380`, `nexusd` **`8085`**
  (docs say `8080`/`8055`; the code is `8085` — see spec Known deviation 3).

```sh
make up && make migrate && make seed          # postgres + pgbouncer + redis; apply migrations; seed tenant "acme" + price book
make run                                      # signerd + nexusd --dev (fake provider, auto-generated keys in .dev/)
export NEXUS_TOKEN=$(make -s token)           # dev bearer token for tenant acme
./bin/nexusctl run "say hello"                # smoke
```

`make migrate` prints that row-level security is enabled on every tenant table. Without `--dev`, `nexusd serve` **fails closed**, listing every
unset security-critical setting in one error (US9).

> **Trap**: a stray container (`nexus-agent-demo-nexusd-1`) can publish the port and silently shadow the native `nexusd` — a 404 on a new route or
> a 401 on a fresh token usually means that, not a regression. Check `docker ps` first.

**Always-available check (no external services)**: `go test ./...` runs unit + property tests. **Integration** (needs Docker; starts its own
Postgres + PgBouncer + Redis via testcontainers — `make up` is *not* required): `go test -race -tags=integration ./...`.

## 1. Foundations — isolation, immutability, erasure-safe replay

| Scenario | How | Check |
|---|---|---|
| Cross-tenant read through the pooler fails; flipping `set_config(…, true)`→`false` makes it fail loudly | `go test -tags=integration ./internal/store -run Isolation -v` | **Auto** (`internal/store/isolation_integration_test.go`) — SC-009 |
| `events` rejects UPDATE/DELETE for every role | integration suite | **Auto** |
| Import boundaries (transitive) and the 500-line cap | `go test ./tests/contract -run 'TestImportBoundaries\|TestGoFilesStayUnderLineCap' -v` | **Auto** — FR-012, FR-014 |
| Telemetry never carries a content-bearing key | `go test ./internal/obs -run Allowlist -v` | **Auto** — SC-014 |
| Content-bearing traces (`NEXUS_TRACE_CONTENT=true`) | `TestKernelTracerContentOptIn` | **Auto, but it asserts the *violation*** — spec Known deviation 1 |

## 2. US1 — run an agent task

```sh
curl -s -X POST localhost:8085/v1/runs -H "authorization: Bearer $NEXUS_TOKEN" -d '{"input":"hello"}'   # → 202 {"run_id":"…"}
curl -sN localhost:8085/v1/runs/<run_id>/events -H "authorization: Bearer $NEXUS_TOKEN"                  # SSE: delta… content … terminal
```

| Scenario | Check |
|---|---|
| Accepted `202`; events stream in order; exactly one typed `terminal` | **Auto** `TestRESTRunEndToEnd` |
| Every `tool_use` has exactly one paired `tool_result` over *generated* histories | **Auto** `tests/property` `TestHygienePairedResultInvariant` — SC-001 |
| Kill the process mid-tool-call; the log still pairs every call | **Auto** `TestRunctl_Resume_RecoversAToolUseAKilledWorkerLeftOrphaned`; manual with `kill -9` on the worker |
| Prefix bytes identical across turns; a one-byte change fails | **Auto** `TestPrefixBytesEquality`, `TestPrefixBytesCatalogOrderIndependent` — SC-002 |
| Cache-read ≥ 90% steady-state | **Auto** `TestCacheReadRate` computes the rate from per-class counts; the ≥ 90% figure itself is a **go-live measurement** (`make go-live`), not a unit assertion — SC-003 |
| Truncated / malformed / throttled stream → typed outcome | **Auto** eval corpus `negative_*.yaml` + `provider/fake` tests |
| REST and CLI produce identical event sequences and terminal reasons; `git diff kernel/` empty after adding a surface | **Auto** `TestRESTAndCLI_ProduceIdenticalEventSequencesAndTerminalReason` — SC-018 |

## 3. US2 — governed tool use

```sh
./bin/nexusctl run --autonomy=read_only  "delete the build dir"   # refused at the autonomy layer: typed reason, audited
./bin/nexusctl run --autonomy=supervised "delete the build dir"   # suspends on an approval
```

| Scenario | Check |
|---|---|
| Ten-layer total order over the layer cross-product | **Auto** `TestChainCrossProduct` |
| A standing scope cannot cause layer 6 or 7 to be skipped | **Auto** `TestChain_StandingScopeCannotSkipLayer6Or7` — SC-005 |
| A hook returning ALLOW is treated as DEFER; rewrites outside the allowlist are refused | **Auto** `TestDispatch_HookAllowIsCoercedToDefer`, `TestDispatch_UpdatedInputOutsideAllowlistIsRefused` |
| Permission denial terminates `permission_denied`; ASK suspends | **Auto** `TestKernel_PermissionDeniedTerminatesRun`, `TestKernel_AwaitingApprovalSuspendsRun` — SC-004 |
| Autonomy cannot be widened | **Auto** `TestRunctl_TightenAutonomy_RefusesWidening` |
| Rule-of-Two taint survives a process restart | **Auto** `TestRuleOfTwo_TaintStateSurvivesASimulatedProcessRestart` |
| `web_crawl` taint equals `web_fetch` taint field for field | **Auto** `internal/tools/builtin/web_crawl_test.go` |

## 4. US3 — cost ceilings

```sh
./bin/nexusctl run --budget=0.05 "…"     # ends cost_exhausted BEFORE the overspending call; budget_decision names the refusing budget
```

| Scenario | Check |
|---|---|
| 20 concurrent sessions never exceed one tenant ceiling | **Auto** `TestCostGate_ConcurrentSessionsRespectOneTenantCeiling` — SC-006 |
| Unreported usage reconciles at the full reserved worst case | **Auto** `TestCostGate_UnreportedUsageReconcilesAtFullReservedCost` |
| Every provider/embedder call passes the gate (AST check) | **Auto** `TestEveryProviderStreamCallIsMetered`, `TestEveryEmbedderEmbedCallIsMetered` — SC-032 |
| Price-book version/effective-range resolution | **Auto** `internal/cost/pricebook_test.go`; reproducing a *historical* call to the exact minor unit (SC-008) has **no end-to-end check** |
| A `cost_exhausted` run names the refusing budget | **Manual** (no named test) — SC-007 |

## 5. US4 — trust surface

```sh
./bin/nexusctl approvals list && ./bin/nexusctl approvals show <id>      # digests, never a bare UUID
./bin/nexusctl approvals grant <id> --modify '{…}'                       # the approver's input executes (→ granted_modified)
make verify-chain                                                        # audit chain has no break or gap
```

| Scenario | Check |
|---|---|
| Modified grant executes the approver's input | **Auto** `TestOversightApproval_GrantModifiedExecutesApproverInput` — SC-012 |
| Substituting an argument after the grant → typed mismatch | **Auto** `TestOversightApproval_MismatchRefusesExecution` — SC-012 |
| Simulated consent has no effect | **Auto** `TestOversightApproval_SimulatedConsentHasNoEffect` — SC-025 |
| Cancel releases a pending approval and aborts | **Auto** `TestRunctl_Cancel_ReleasesPendingApprovalAndAborts` |
| After erasure the log replays structurally **and** the chain verifies | **Auto** `TestErasure_StructuralReplayAndChainSurviveShredding` — SC-010 |
| The verifier **reports** a break/gap when the chain is tampered with | **No check** — only clean-chain verification is tested; no test injects tampering and expects a `Break`/`Gap` — SC-011 |
| Suspension costs zero tokens; suspended time excluded from latency | **No direct assertion found** — SC-013 |

## 6. US5 — reliability

| Scenario | Check |
|---|---|
| `kill -9` mid-tool-call → job reclaimed, resumes from checkpoint; ambiguous effect never re-executed | **Auto** `TestQueue_ReclaimsAbandonedLeaseAfterIdleTimeout`, `TestClaims_EndToEnd_NeverReExecutesAnAmbiguousEffect`, `TestRunctl_Resume_RefusesWhileAClaimIsUnresolved` — SC-015 |
| Per-session serial, cross-session concurrent | **Auto** `TestSessionLock_SerialPerSessionConcurrentAcrossSessions` |
| Deleting every snapshot changes only hydration time | **Auto** `TestSnapshot_DeletingAllSnapshotsChangesNothingButHydrationTime` — SC-016 |
| Replay is pure and agrees with hydration | **Auto** `TestRunctl_Replay_IsPureAndAgreesWithHydration` |
| Second corroborating stuck trip terminates | **Auto** `TestStuckDetection_SecondCorroboratingTripTerminates` |
| Conversational session: same `session_id` over 3 turns, no `terminal` until cancel | **Auto** `TestRESTConversationalSession_MultiTurnSameSessionID` |
| **`nexusctl fork <session> --at 42 --model haiku`** | **No check** — no test calls `Control.Fork`; the only reference is a "Fork not wired" stub. Manual only — SC-017 |

## 7. US6 — memory, skills, surfaces

| Scenario | Check |
|---|---|
| Memory injected at session start, with a leading audit event | **Auto** `TestMemory_InjectedAtSessionStart_ProducesLeadingAuditEvent` |
| Revoke a skill's declared tool mid-session → `skill_capability_ignored`, run continues | **Auto** `TestSkills_MidSessionToolRevocation_EmitsCapabilityIgnoredAndContinues` — SC-019 |
| Outbox retries then delivers, idempotently; fails permanent after a cap and never resends | **Auto** `TestOutbox_RetriesThenDeliversAndStaysIdempotent`, `TestOutbox_FailsPermanentlyAfterCapAndNeverSendsAgain` — SC-020 |

## 8. US7 — plans and delegation

| Scenario | Check |
|---|---|
| Zero-token routing: no `Provider.Stream` during transition evaluation | **Auto** `TestPlan_ZeroTokenRouting` — SC-023 |
| Plan eval gate advances lifecycle; no tagged cases fails closed | **Auto** `TestPlanEvalGate_*` |
| The same plan and input run twice take the same branch, and the transition log names the predicate that fired | **No named check** — `TestPlan_ZeroTokenRouting` covers the zero-token half; determinism of the branch is not asserted by name (T249) — SC-024 |
| Delegation round trip; bounds fail closed through the real chain | **Auto** `TestDelegate_RoundTrip`, `TestDelegate_BoundsFailClosed_ThroughTheRealChain` — SC-022 |
| A child cannot return a clean-looking summary (taint ascends from the child's own events) | **Auto**, per `phase8_orchestration_test.go`'s header ("taint-ascend fold across a real spawn/resolve round trip"); no test is *named* for it — SC-021 |

## 9. US8 — the release gate and go-live

```sh
make eval                         # per-case table, intervals, 3-valued verdict → PASS (n/m, 0 regressions, efficiency within band) or a red gate
make go-live TENANT=acme          # automated items verified; manual items printed as reminders
make dashboard                    # golden signals per tenant
```

| Scenario | Check |
|---|---|
| Prompt change holding quality but raising tokens 40% is **blocked** | **Auto** `evals/gate_test.go`, `evals/regression_test.go` — SC-026 |
| ≥ 90% pass and zero regressions, or the merge is blocked | **Auto in CI** — the `eval-gate` job runs `make eval` against `evals/testdata/baseline.json` — SC-027 |
| Golden signals compute correctly | **Auto** `TestComputeGoldenSignals` |
| `inconclusive` never resolves to `pass`; held-out gap measured | **Auto** `evals/{stats,grading,policy}_test.go` |

## 10. US9 — production readiness

| Scenario | Check |
|---|---|
| No bearer token → 401, never routed; readiness only when Postgres/Redis/consumer group/signerd answer | **Auto** `internal/surfaces/rest/authn_test.go`; readiness **Manual** (`curl localhost:8085/readyz`) — SC-028 |
| `nexusd serve` with no env and no `--dev` exits with one combined error | **Auto** `internal/config/config_test.go`; **Manual** to see it end to end |
| Measured submission rate on the fake provider | **Auto** `TestLoadPOSTRunsThroughput` — SC-034 |
| Container is non-root; sandbox has dropped caps, read-only root | **Manual**: `docker run --rm <image> id`; `docker inspect` a sandbox container |
| `make docker-up` runs `nexusd` + `signerd` from built images | **Manual** |

## 11. US10 – US14 (opt-in)

| Scenario | Check |
|---|---|
| Two members race for one card; exactly one wins | **Auto** `TestClaimCard_ContentionRace_ExactlyOneWinner` — SC-029 |
| Clean reader of a tainted card is tainted; a flagged card is never surfaced | **Auto** `TestReadBoard_FoldsTaintFromCleanCard_NeverFromFlagged` |
| Nested team creation denied | **Auto** `TestCreateTeam_DeniesNestedTeamCreation` |
| A live OAuth token never appears in an event, log line or span | **Auto** `TestSecretLeak_LiveOAuthTokenNeverAppearsInOutputOrLogs` — SC-030 |
| Same task via Telegram / email / web = same events as REST | **Manual** with real or stubbed webhooks; per-surface dispatch tests are **Auto** |
| Ingest → retrieve → erase leaves the index empty; rejected doc indexes nothing; tenant-isolated | **Auto** `TestRetrieval_IngestIndexSearchErase`, `TestRetrieval_RejectedDocumentIndexesNothing`, `TestRetrieval_TenantIsolation` — SC-031 |
| Crawl + opensandbox end to end | **Manual**: `make agentic-up`, then `nexusctl run "research the top 3 headlines on https://news.ycombinator.com and summarize them"`; stop it and confirm `platform/shell` still runs with a warning (SC-033) |
| Observability stack | **Manual**: `make docker-up && make observability-up`; Prometheus targets `nexusd`/`cadvisor`/`docker-label-exporter` `UP` (`curl -s localhost:9091/api/v1/targets`); five rules healthy (`/api/v1/rules`); Alertmanager ready (`localhost:9094/api/v2/status`); Loki `ready` (`localhost:3101/ready`); stop `nexusd` and `NexusTargetDown` fires within a minute |

## Coverage summary

Of the 34 success criteria: most have a named automated test; **SC-007, SC-008, SC-011, SC-013, SC-017** have none or only partial automation (marked
above), and **SC-003's ≥ 90% figure**, **SC-028's readiness**, **SC-033** and the whole observability stack are manual or go-live measurements. These are
carried into Phase 19 of [tasks.md](tasks.md).
