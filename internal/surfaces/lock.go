package surfaces

import (
	"context"
	"time"
)

// Locker is the structural seam every conversational webhook surface
// (telegram/zalo/email dispatch) locks a conversation's decide-then-act
// sequence through — *internal/queue.SessionLock's own Acquire/Release
// shape, duplicated here exactly like internal/queue.Locker itself is
// duplicated in internal/delegate/spawn.go: this package must not import
// internal/queue (a data-plane package) for the same reason
// internal/surfaces/rest holds its own RunStarter instead of importing
// kernel.
//
// The key is the surface's own session_key, not the session id the queue
// workers lock on for the length of a run. Sharing the worker's key would
// make every delivery for a conversation wait out the turn already running
// for it, which is the wrong failure mode: what this lock prevents is two
// deliveries each deciding "no session is awaiting input" at the same
// moment and both starting a fresh one.
type Locker interface {
	Acquire(ctx context.Context, sessionKey string) (token string, ok bool, err error)
	Release(ctx context.Context, sessionKey, token string) error
}

// lockAcquireAttempts/lockAcquireDelay bound how long a synchronous webhook
// handler contends for a session's lock before giving up — the same
// short-retry cadence internal/queue.Worker's own contended-lock path
// uses, just retried locally: a handler holds a provider's HTTP request
// open and has no way to hand a delivery back for later. Total worst case
// (~1.2s of sleeping across 5 attempts) stays well inside every provider's
// own webhook response timeout.
const (
	lockAcquireAttempts = 5
	lockAcquireDelay    = 300 * time.Millisecond
)

// AcquireSessionLock retries Locker.Acquire on the fixed cadence above.
// ok=false after every attempt means another goroutine — same process or a
// sibling nexusd pod — is ALREADY handling a delivery for this exact
// session_key; what to do about that is the
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
