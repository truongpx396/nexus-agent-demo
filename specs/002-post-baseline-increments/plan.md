# Implementation Plan: Post-baseline increments: webhook idempotency, hexagonal architecture and a provable topology seam

**Spec**: [spec.md](spec.md) | **Status**: Implemented (retroactive)

> **Retroactive.** This spec was reconstructed from Phases 18, 19 and 20 of the build ledger (`docs/build-phases.md`, deleted; frozen at `09d54f8`), which it replaces. The ledger's text is carried over, not paraphrased; each ledger row is a task in [tasks.md](tasks.md) and keeps its id as `(bp N.M)`, which is what `task N.M` citations in the code resolve to. See [../README.md](../README.md).

## Summary

Three increments after the baseline: (18) a provider's redelivered webhook, or two concurrent messages on one conversation, could double-run a paid LLM turn;
(19) the core depended on Postgres and on five copies of the run-admission sequence rather than on ports; (20) the topology framing rested on three claims that
nothing checked: a contract that was Go types only, a session lock that lapsed mid-run, and container images that no CI job built.

## Background

### Ledger Phase 18: Inbound-delivery dedup, webhook session locking and atomic conversation resume

Not part of the spec's 67-pattern coverage — nothing here is a new architectural idea, and like
Phase 13 it closes a gap between claimed and shipped behavior rather than adding one. Driven by
[`docs/webhook-concurrency-review.md`](../../docs/webhook-concurrency-review.md) (2026-09-18): Phase 11's
conversational webhook surfaces (Telegram/Zalo/email) each did an unlocked check-then-act against
`store.GetSessionByKey`/`ResumeConversation`, and nothing deduped a provider's own redelivered
webhook, so a duplicate or concurrent delivery could double-run a paid LLM turn. 18.1–18.3 close
that on the webhook surfaces. 18.4 closes the same check-then-act inside `ResumeConversation`
itself, which also covers REST's steer endpoint.

The review also proposed moving the queue to a Redis Streams consumer group. That shipped
separately (every run is queued; see Phase 6 and `internal/queue/redis_streams.go`) and is not part
of this phase.

### Ledger Phase 19: Hexagonal architecture: persistence and use-case seams

Not part of the spec's 67-pattern coverage and, like Phases 13 and 18, nothing here is a new
architectural idea: it closes the gap between "every seam is a Go interface" and a core that
actually has no knowledge of its adapters. An architecture review (2026-10-29) found
ports-and-adapters applied at the volatile edges (`provider.Provider`, `queue.Port`,
`tools.SandboxExec`, `audit.Signer`, `controlplane.Port`) but not at persistence or the use-case
layer: `kernel.Kernel` held a concrete `*store.Store` and passed `pgx.Tx` through its hooks; every
surface opened tenant transactions itself; the session-plus-DEK admission sequence existed five
times (REST, Telegram, Zalo, email, cron) together with five copies of `RunRequest`/`RunEvent`/
`SealFunc`/`RunStarter`; and the kernel loop could only be exercised against Postgres. Nothing was
deployed, so each task below is a straight refactor with no data migration.

### Ledger Phase 20: A provable topology seam, and a session lock that holds

Not part of the spec's 67-pattern coverage and, like Phases 13, 18 and 19, nothing here is a new
architectural idea. Reframing the README around deployment topology and comparing it with the spec's
[Deployment topologies](https://github.com/truongpx396/nexus-agent-spec#deployment-topologies) found three claims that
rested on nothing that checked them: the control↔data-plane contract (#62) was Go types with no wire encoding and
only `AdmitRun` ever crossed it; "any worker can take any job" depended on a session lock that was never renewed
(`SessionLock.Renew` existed, and nothing outside its own test called it), so serial-per-session lapsed after the
lock's 30 seconds even in one process; and the container images were built by nothing in CI, and had stopped
building when `go.mod` moved past the golang image's Go. Nothing is deployed, so each task is a straight change
with no data migration.

## Constitution Check

The ledger records each phase as adding no new architectural idea and no new principle. The constitution (`.specify/memory/constitution.md`) is the review
checklist for any later change.
