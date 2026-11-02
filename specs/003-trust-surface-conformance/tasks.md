---

description: "Task list for trust-surface conformance (proposed)"
---

# Tasks: Trust-surface conformance

**Input**: [spec.md](spec.md), [plan.md](plan.md)

**Paths**: verified at HEAD (`tests/contract/specs_test.go` checks the path in every `[X]` task; these are all open, so the paths named are the intended locations).

> **Moved here from spec 001.** These tasks were T232–T238 in the Convergence backlog of [spec 001](../001-agent-platform/tasks.md), which `/speckit-converge` appended against `8e02491`. Each is re-checked against the code on 2026-11-02 and still open. The backlog ids are kept in brackets so history stays traceable.

## Format: `- [ ] T### [US#] Description in path (was 001 T###)`

## Phase 1: User Story 2 - telemetry has no content channel (a violation, so first)

- [ ] T001 [US2] Decide the resolution (remove `NEXUS_TRACE_CONTENT` and `Span.SetContent`, or amend the constitution with a dev-only channel that is refused outside `--dev` and emits a receipt), and implement it in `cmd/nexusd/serve.go`, `internal/obs/otlp.go`, `internal/obs/tracer.go` (was 001 T232)
- [ ] T002 [US2] Change `TestKernelTracerContentOptIn` in `tests/integration/tracer_test.go` to assert the new behaviour instead of the violation (was 001 T232)

## Phase 2: User Story 1 - webhooks (replay window, per-sender limits)

- [ ] T003 [US1] Reject a delivery outside a bounded replay window before the kernel sees it, per provider, in `internal/surfaces/telegram/webhook.go`, `internal/surfaces/zalo/webhook.go`, `internal/surfaces/email/webhook.go` (was 001 T233)
- [ ] T004 [US1] Key the rate limiters by external identity (Telegram `chat_id`, Zalo sender id, email `from`) with the tenant limit kept as a ceiling, in `internal/surfaces/telegram/ratelimit.go`, `internal/surfaces/zalo/ratelimit.go`, `internal/surfaces/email/ratelimit.go` (was 001 T234)
- [ ] T005 [US1] Test that a replayed, validly signed delivery is rejected before a run starts, and that one sender cannot exhaust another's allowance, in `internal/surfaces/telegram/webhook_test.go`

## Phase 3: User Story 3 - audit anchoring and alerting

- [ ] T006 [US3] Anchor the chain head in an external append-only sink behind a port, and have the verifier cross-check it, in `internal/audit/postgres/anchor.go`, `internal/audit/postgres/verify.go`, `cmd/nexusd/background.go` (was 001 T235)
- [ ] T007 [US3] Expose a break-or-gap counter on `/metrics` and add a Prometheus rule that names a runbook, in `internal/obs/dashboard.go`, `deploy/observability/prometheus-rules.yml` (was 001 T236)
- [ ] T008 [US3] Test that deleting a chain's tail together with its anchors is reported as a break, in `tests/integration/audit_tamper_test.go` (relates to 001 T245)

## Phase 4: User Story 4 - harness digest pinning

- [ ] T009 [US4] Derive the system-prompt, safety-policy and approval-policy versions from their content and set them at every call site, in `internal/runs/launch.go`, `internal/runctl/fork.go`, `internal/surfaces/cron/scheduler.go` (was 001 T237)
- [ ] T010 [US4] Test that the digest changes when the safety policy, approval policy or system prompt changes, and is stable otherwise, in `internal/harness/digest_test.go` (was 001 T237)

## Phase 5: User Story 5 - egress masking

- [ ] T011 [US5] Run `/speckit-specify` for class-based egress masking (PII, secrets, PHI, card data) and resolve its open questions (was 001 T238)
- [ ] T012 [US5] Implement a masking stage in front of provider calls and outbound deliveries in `internal/provider/provider.go`, `internal/surfaces/outbox.go` (was 001 T238)
- [ ] T013 [US5] Clear the eval gate (`make eval`) and add a recording-provider test that a masked class never arrives unmasked (was 001 T238)
