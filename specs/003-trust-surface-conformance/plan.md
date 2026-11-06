# Implementation Plan: Trust-surface conformance

**Spec**: [spec.md](spec.md) | **Status**: Draft (proposed)

## Summary

Close five constitution conformance gaps in the order that changes what is *allowed* first: the telemetry content channel (a violation), then the three security gaps (webhook replay and per-sender limits, audit anchoring and alerting, digest pinning), then egress masking, which is the largest and needs its own specification pass.

## Technical Context

Go 1.26, Postgres through PgBouncer, Redis. Everything below already exists and is changed, not added:

- **Webhooks**: `internal/surfaces/{telegram,zalo,email}/webhook.go` verify the provider and call `RateLimit.Allow(tenantID.String())`; `ratelimit.go` in each is an in-memory token bucket; `internal/surfaces/dedup.go` is the delivery-id dedup.
- **Telemetry**: `internal/obs/{allowlist,tracer,otlp,langfuse,multi}.go`; the content opt-in is `kernel.Kernel.TraceContent`, set from `NEXUS_TRACE_CONTENT` in `cmd/nexusd/serve.go`.
- **Audit**: `internal/audit/postgres/{chain,anchor,verify}.go`, the verifier loop in `cmd/nexusd/background.go`, `internal/reporting/golden_signals.go` for the golden signals, `deploy/observability/prometheus-rules.yml` for the five alert rules.
- **Digest**: `internal/harness/digest.go`, called from `internal/runs/launch.go`, `internal/runctl/fork.go` and `internal/surfaces/cron/scheduler.go`.
- **Egress**: `internal/provider/` (one port, three adapters) and `internal/surfaces/outbox.go`.

## Constitution Check

This spec *is* conformance work, so the check is the reverse of usual: each story cites the clause it closes (Security & Trust Surface: inbound channel authenticity; observability captures structure, not content; audit receipts; egress and redaction; state artifacts and the digest). No principle is redefined, except that User Story 2 may propose a MINOR amendment if the content channel is kept.

## Order and dependencies

1. **US2** first: it is a violation, and its resolution (remove the flag, or amend the constitution) is a decision, so it blocks nothing but should not wait.
2. **US1, US3, US4** are independent of each other. US3 depends on the external-sink decision; US4 is the smallest change.
3. **US5** last. It touches every provider call and every outbound delivery, so it wants the interface decided first (a masking stage in front of `provider.Provider` and the outbox), and a full `/speckit-specify` pass.

## Risks

- **A per-sender limit needs a sender identity per provider** (`chat_id`, the Zalo sender id, the email `from`); an attacker who varies it defeats the limit. The tenant-wide limit should stay as a ceiling above the per-sender one.
- **The external anchor is a new dependency.** It should sit behind a port, like `signerd` does, so the demo can keep a local implementation.
- **Masking changes what the model sees**, so it is behaviour-bearing and has to clear the eval gate (`make eval`) like a prompt change.
