package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// SessionLock is the per-session-key Redis serial lock README task 6.2
// names: at most one worker may hold session_key S's lock at a time (serial
// per session), while N distinct session_keys are held concurrently across
// the whole worker pool without contending each other at all (cross-session
// concurrent) — the cross-WORKER analogue of internal/tools/pipeline.go's
// existing in-process sessionState.serialLock, whose own doc comment names
// this exact gap: "There is no cross-worker lock yet."
//
// Implemented with Redis SET NX PX (acquire) and a Lua compare-and-delete
// (release) — the same care internal/cost/redis.go's Lua scripts take to
// stay atomic under concurrent callers: a release must only ever remove a
// lock THIS holder still owns, never one another worker already acquired
// after this holder's lease expired and got reaped.
type SessionLock struct {
	client *redis.Client
	ttl    time.Duration
}

func NewSessionLock(client *redis.Client, ttl time.Duration) *SessionLock {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &SessionLock{client: client, ttl: ttl}
}

func lockKey(sessionKey string) string { return "nexus:queue:session_lock:" + sessionKey }

// Acquire never blocks waiting for contention — a worker that fails to
// acquire (ok=false) simply leaves its leased job for a later retry rather
// than parking a goroutine, matching internal/queue/worker.go's own
// non-blocking poll loop. token identifies THIS acquisition; only Release/
// Renew calls carrying the same token can affect it.
func (l *SessionLock) Acquire(ctx context.Context, sessionKey string) (token string, ok bool, err error) {
	tok, err := randomToken()
	if err != nil {
		return "", false, fmt.Errorf("queue: generate lock token: %w", err)
	}
	set, err := l.client.SetNX(ctx, lockKey(sessionKey), tok, l.ttl).Result()
	if err != nil {
		return "", false, fmt.Errorf("queue: acquire session lock %s: %w", sessionKey, err)
	}
	if !set {
		return "", false, nil
	}
	return tok, true, nil
}

// luaCompareAndDelete: KEYS[1]=lock key, ARGV[1]=expected token. Only
// deletes if the current value still matches — the standard Redlock-style
// safe-release pattern, so a worker that took so long its lease already
// expired (and was reacquired by someone else) can never delete a stranger's
// lock.
const luaCompareAndDelete = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`

func (l *SessionLock) Release(ctx context.Context, sessionKey, token string) error {
	if err := l.client.Eval(ctx, luaCompareAndDelete, []string{lockKey(sessionKey)}, token).Err(); err != nil {
		return fmt.Errorf("queue: release session lock %s: %w", sessionKey, err)
	}
	return nil
}

// luaCompareAndExtend: KEYS[1]=lock key, ARGV[1]=expected token,
// ARGV[2]=new TTL milliseconds. Same ownership check as release, but PEXPIRE
// instead of DEL — for a turn loop that outlives the lock's own TTL and
// needs to keep renewing it rather than have another worker steal the
// session mid-run.
const luaCompareAndExtend = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`

func (l *SessionLock) Renew(ctx context.Context, sessionKey, token string) (bool, error) {
	res, err := l.client.Eval(ctx, luaCompareAndExtend, []string{lockKey(sessionKey)}, token, l.ttl.Milliseconds()).Result()
	if err != nil {
		return false, fmt.Errorf("queue: renew session lock %s: %w", sessionKey, err)
	}
	n, _ := res.(int64)
	return n == 1, nil
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
