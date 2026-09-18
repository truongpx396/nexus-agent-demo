package surfaces

import (
	"context"
	"time"
)

// Locker is the structural seam every conversational webhook surface
// (telegram/zalo/email dispatch) and REST's own steer endpoint lock a
// session's turn through — *internal/queue.SessionLock's own
// Acquire/Release shape, duplicated here exactly like internal/queue.Locker
// itself is duplicated in internal/delegate/spawn.go: this package must not
// import internal/queue (a data-plane package) for the same reason
// internal/surfaces/rest holds its own RunStarter instead of importing
// kernel. cmd/nexusd wires the SAME *queue.SessionLock instance (same Redis
// client, same TTL — lockKey's own derivation is fixed, not per-instance,
// so two separate values constructed the same way ARE the same lock) into
// every one of these callers, so a webhook-driven resume and a REST-driven
// steer of the SAME session_key contend for the SAME Redis key — closing
// the production-readiness review's own finding: "the REST/webhook
// direct-call path bypasses [SessionLock] entirely."
type Locker interface {
	Acquire(ctx context.Context, sessionKey string) (token string, ok bool, err error)
	Release(ctx context.Context, sessionKey, token string) error
}

// lockAcquireAttempts/lockAcquireDelay bound how long a synchronous HTTP
// handler contends for a session's lock before giving up — the same
// short-retry cadence internal/queue.Worker's own contended-lock path
// already uses (worker.go: "another worker already holds this session's
// serial slot — put the job back for a quick retry"), just retried locally
// instead of via a queue requeue, since a webhook/REST handler has no
// requeue mechanism of its own to fall back on and queue_jobs.payload must
// never carry a plaintext opening message (cmd/nexusd/background.go's own
// doc comment on why fresh/resumed turns stay off the queue). Total worst
// case (~1.2s of sleeping across 5 attempts) stays well inside every
// provider's own webhook response timeout.
const (
	lockAcquireAttempts = 5
	lockAcquireDelay    = 300 * time.Millisecond
)

// AcquireSessionLock retries Locker.Acquire on the fixed cadence above.
// ok=false after every attempt means another goroutine — same process, a
// sibling nexusd pod, or the crash-recovery queue worker — is ALREADY
// driving this exact session_key's turn; what to do about that is the
// caller's own documented choice, never this function's.
func AcquireSessionLock(ctx context.Context, l Locker, sessionKey string) (token string, ok bool, err error) {
	for attempt := 0; attempt < lockAcquireAttempts; attempt++ {
		token, ok, err = l.Acquire(ctx, sessionKey)
		if err != nil || ok {
			return token, ok, err
		}
		if attempt == lockAcquireAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-time.After(lockAcquireDelay):
		}
	}
	return "", false, nil
}
