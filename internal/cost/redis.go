package cost

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// The tenant-level ceiling (BudgetScopeTenant) is enforced across every
// worker/goroutine touching one tenant, so it needs one atomic counter
// shared outside any single process — Redis, driven entirely through Lua
// scripts so a check-then-increment-then-ceiling-compare is one atomic
// round trip, never three racy ones (task 4.4).
//
// Each budget owns two keys: an EPOCH key (a generation marker) and a
// SPEND key scoped to that epoch. Reserve's script refuses to act at all
// if the epoch key is missing or doesn't match the caller's expectation —
// "unknown epoch = unavailable = fail closed (never 'no spend yet')"
// (task 4.4) — rather than silently treating a cold/flushed Redis as zero
// spend. The epoch key is only ever created by Arm (gate.go), which
// rehydrates the spend counter from Postgres's own durable cost_records
// history first — so even a full Redis flush recovers to the TRUE spend,
// not to zero.

func epochKey(budgetID uuid.UUID) string { return "nexus:cost:budget:" + budgetID.String() + ":epoch" }

func spendKey(budgetID uuid.UUID, epoch int64) string {
	return fmt.Sprintf("nexus:cost:budget:%s:spend:%d", budgetID, epoch)
}

// reserveOutcome is Reserve's typed script result — deliberately distinct
// from DecisionKind (decision.go): a caller folds reserveUnavailable and
// reserveOverCeiling into the same DecisionRefuseCeiling (both mean
// "refuse"), but the two are logged with different reasons.
type reserveOutcome int

const (
	reserveUnavailable reserveOutcome = iota // epoch key missing (never armed, or Redis was flushed)
	reserveStaleEpoch                        // epoch key present but doesn't match the caller's expected generation
	reserveOverCeiling
	reserveOK
)

// luaReserve: KEYS[1]=epoch key, KEYS[2]=spend key. ARGV[1]=expected
// epoch, ARGV[2]=amount to reserve, ARGV[3]=ceiling. Returns {code, total}
// where total is the resulting spend counter (or the epoch on a mismatch,
// or 0 when unavailable) — atomic: the ceiling compare-and-rollback can
// never race against a concurrent Reserve the way separate GET+INCR+GET
// calls would.
const luaReserve = `
local epoch = redis.call('GET', KEYS[1])
if not epoch then
  return {-1, 0}
end
if epoch ~= ARGV[1] then
  return {-2, tonumber(epoch)}
end
local newTotal = tonumber(redis.call('INCRBY', KEYS[2], ARGV[2]))
if newTotal > tonumber(ARGV[3]) then
  redis.call('DECRBY', KEYS[2], ARGV[2])
  return {0, newTotal - tonumber(ARGV[2])}
end
return {1, newTotal}
`

// luaRelease: KEYS[1]=epoch key, KEYS[2]=spend key. ARGV[1]=expected
// epoch, ARGV[2]=delta to release (DECRBY; a negative delta increments,
// covering the "actual cost exceeded the reservation" case). A stale
// epoch is a documented no-op (redis.go's package doc comment) — nothing
// this phase ever advances a live budget's epoch, so it is never actually
// exercised outside a test that deliberately re-arms one.
const luaRelease = `
local epoch = redis.call('GET', KEYS[1])
if not epoch or epoch ~= ARGV[1] then
  return 0
end
redis.call('DECRBY', KEYS[2], ARGV[2])
return 1
`

// luaArm: KEYS[1]=epoch key, KEYS[2]=spend key. ARGV[1]=epoch to
// establish, ARGV[2]=baseline spend (Postgres's own reconciled total —
// gate.go's Arm, never zero-on-faith). A no-op if the epoch key already
// exists: arming must never clobber a counter another still-live process
// is actively reserving against.
const luaArm = `
local existing = redis.call('GET', KEYS[1])
if existing then
  return 0
end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('SET', KEYS[2], ARGV[2])
return 1
`

// scripter is the minimal Redis surface Gate needs. The production
// implementation (redisScripter below) wraps a real *redis.Client;
// gate_integration_test.go exercises it against a real Redis container —
// the whole point of the Lua atomicity is only proven against the real
// thing, so this package does not also carry an in-memory fake reimplementing
// the same semantics for a plain `go test` run (internal/crypto's
// KeyStore, the nearest precedent in this codebase, follows the same rule:
// Postgres-touching methods are integration-tested only).
type scripter interface {
	Reserve(ctx context.Context, budgetID uuid.UUID, epoch, amount, ceiling int64) (reserveOutcome, int64, error)
	Release(ctx context.Context, budgetID uuid.UUID, epoch, delta int64) error
	Arm(ctx context.Context, budgetID uuid.UUID, epoch, baseline int64) error
}

type redisScripter struct {
	client  *redis.Client
	timeout time.Duration
}

func newRedisScripter(client *redis.Client, timeout time.Duration) *redisScripter {
	if timeout <= 0 {
		timeout = 200 * time.Millisecond
	}
	return &redisScripter{client: client, timeout: timeout}
}

func (s *redisScripter) Reserve(ctx context.Context, budgetID uuid.UUID, epoch, amount, ceiling int64) (reserveOutcome, int64, error) {
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	res, err := s.client.Eval(cctx, luaReserve, []string{epochKey(budgetID), spendKey(budgetID, epoch)}, epoch, amount, ceiling).Result()
	if err != nil {
		// A Redis error (timeout, connection refused, ...) is exactly as
		// fail-closed as a missing epoch key: the gate cannot prove
		// there's room, so it must not assume there is.
		return reserveUnavailable, 0, fmt.Errorf("redis reserve: %w", err)
	}
	pair, ok := res.([]interface{})
	if !ok || len(pair) != 2 {
		return reserveUnavailable, 0, fmt.Errorf("redis reserve: unexpected script result %#v", res)
	}
	code := toInt64(pair[0])
	total := toInt64(pair[1])
	switch code {
	case -1:
		return reserveUnavailable, 0, nil
	case -2:
		return reserveStaleEpoch, total, nil
	case 0:
		return reserveOverCeiling, total, nil
	case 1:
		return reserveOK, total, nil
	default:
		return reserveUnavailable, 0, fmt.Errorf("redis reserve: unexpected status code %d", code)
	}
}

func (s *redisScripter) Release(ctx context.Context, budgetID uuid.UUID, epoch, delta int64) error {
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.client.Eval(cctx, luaRelease, []string{epochKey(budgetID), spendKey(budgetID, epoch)}, epoch, delta).Err(); err != nil {
		return fmt.Errorf("redis release: %w", err)
	}
	return nil
}

func (s *redisScripter) Arm(ctx context.Context, budgetID uuid.UUID, epoch, baseline int64) error {
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.client.Eval(cctx, luaArm, []string{epochKey(budgetID), spendKey(budgetID, epoch)}, epoch, baseline).Err(); err != nil {
		return fmt.Errorf("redis arm: %w", err)
	}
	return nil
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}
