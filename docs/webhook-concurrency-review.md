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

- `migrations/0025_inbound_deliveries.sql` + `store.ClaimInboundDelivery`: the cheap first line of
  defense finding #1 asked for, wired into all three webhook surfaces via
  `surfaces.ClaimDelivery`.
- `surfaces.Locker` + `surfaces.AcquireSessionLock`: a `*queue.SessionLock` is wired into every
  webhook surface's `dispatch()`, closing finding #2 for the webhooks — two racing deliveries for
  one conversation can no longer each decide no session is awaiting input and both start a fresh
  one. The key is the surface's own `session_key`, deliberately not the session id the queue workers
  lock on for the length of a run: sharing it would make every delivery wait out the turn already
  running.
- `store.ClaimAwaitingInput`: finding #2 for REST's steer/resume path (and any other caller of
  `ResumeConversation`) is fixed in the database, not with a lock. The `awaiting_input` → `running`
  flip is now a single compare-and-set, where it used to be a lock-free read followed by a write —
  concurrent callers could all pass the check, each appending a message and queuing a continuation
  (reproduced: 2–4 of 8 concurrent callers succeeded on every run before the change). A Redis lock
  in REST's handler was ruled out because the queue worker holds the session's lock for the whole
  run, so it would have refused every steer of an in-flight run.

The review's original proposal to move the queue to Redis Streams shipped separately and is
unrelated to these findings.
