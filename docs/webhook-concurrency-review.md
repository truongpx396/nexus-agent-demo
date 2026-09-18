# Webhook concurrency review (2026-09-18)

A targeted follow-up to [`docs/production-readiness-review.md`](production-readiness-review.md),
scoped to one question: is this binary ready for real production traffic on its conversational
webhook surfaces (Telegram, Zalo, email) and REST's own steer/resume path? It found the gap
[`docs/build-phases.md`](build-phases.md) Phase 18 closes.

## Where it fell short of "ready for real production traffic"

The concurrency gap wasn't hypothetical edge-casing — Telegram/Zalo webhooks retry by design, and
the code's own comment in `internal/store/append.go` admitted it: "Phase 2 only ever runs one
goroutine per session ... this is the transaction-local belt in the meantime" until a real
distributed lock covers this path. A team shipping this to real users would treat "duplicate
webhook delivery double-runs a paid LLM turn and double-fires side-effecting tools" as a blocking
bug, not a nice-to-have.

1. **No inbound dedup on the provider's own delivery ID** (Telegram `update_id`, Zalo's event id,
   an inbound email's `Message-ID`) as a cheap first line of defense — production bot integrations
   almost always have this. Telegram's `update_id` and email's `Message-ID` were parsed and
   immediately dropped; Zalo's event id wasn't even parsed.

2. **The Redis `SessionLock` (a real distributed lock) existed but only guarded the async
   queue-worker path** — the REST/webhook direct-call path added for conversational continuity
   bypassed it entirely, so horizontal scaling (multiple `nexusd` pods) would make the race easier
   to hit, not harder. `dispatch()` in every webhook surface did an unlocked
   check-then-act (`GetSessionByKey` → branch → `startRun`/`resumeRun`), and REST's own
   `handleSteerRun` had the identical shape against `ResumeConversation`.

3. **The taint-state durability gap (closed by PR #45) sat unnoticed until a tangential review
   thread surfaced it**, for a mechanism explicitly billed as a safety invariant. That suggested the
   "what happens on restart" chaos-testing a safety-critical feature deserves wasn't done when the
   feature first shipped — a process gap this document doesn't re-litigate, since #45 already closed
   the finding itself; it's recorded here only as part of why the concurrency path below got a
   dedicated look rather than being assumed fine by extension.

## What Phase 18 does about it

- `migrations/0024_inbound_deliveries.sql` + `store.ClaimInboundDelivery`: the cheap first line of
  defense finding #1 asked for, wired into all three webhook surfaces via
  `surfaces.ClaimDelivery`.
- `surfaces.Locker` + `surfaces.AcquireSessionLock`: the SAME `*queue.SessionLock` instance the
  crash-recovery queue worker already holds turns through is now wired into every webhook surface's
  `dispatch()` AND REST's `handleSteerRun`, closing finding #2 — a webhook-driven resume and a
  REST-driven steer of the same conversation now contend for the same Redis key, and horizontal
  scaling no longer widens this race.
- The queue backend itself moved from Postgres `SKIP LOCKED` to a Redis Streams consumer group
  (`internal/queue/redisstream.go`), a change adjacent to this review rather than one of its
  findings — see Phase 18's own task table for the independent motivation (continuous crash
  recovery via `XAUTOCLAIM` instead of a startup-only sweep, and removing poll-driven admin-Postgres
  traffic from the lease/complete/fail path).
