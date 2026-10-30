// Package redis is the Redis implementation of connectors.StateStore: a
// bounded-TTL key per in-flight authorization-code flow.
package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/truongpx396/nexus-agent-demo/internal/connectors"
)

const keyPrefix = "nexus:connectors:oauth_state:"

// luaGetAndDelete: single-use retrieval, the same compare-and-consume
// atomicity internal/queue/lock.go's own release script uses for its
// (different) purpose — a replayed callback with an already-consumed state
// finds nothing and fails closed, rather than re-running a stale exchange
// against whatever "code" a replay happens to carry.
const luaGetAndDelete = `
local v = redis.call('GET', KEYS[1])
if v then redis.call('DEL', KEYS[1]) end
return v
`

// States is connectors.StateStore over Redis.
type States struct {
	rdb *goredis.Client
}

// NewStates returns States over rdb.
func NewStates(rdb *goredis.Client) *States { return &States{rdb: rdb} }

var _ connectors.StateStore = (*States)(nil)

// Put implements connectors.StateStore.
func (s *States) Put(ctx context.Context, state string, raw []byte, ttl time.Duration) error {
	if err := s.rdb.Set(ctx, keyPrefix+state, raw, ttl).Err(); err != nil {
		return fmt.Errorf("connectors: store state: %w", err)
	}
	return nil
}

// Take implements connectors.StateStore.
func (s *States) Take(ctx context.Context, state string) ([]byte, bool, error) {
	res, err := s.rdb.Eval(ctx, luaGetAndDelete, []string{keyPrefix + state}).Result()
	if err != nil && !errors.Is(err, goredis.Nil) {
		return nil, false, fmt.Errorf("connectors: consume state: %w", err)
	}
	raw, _ := res.(string)
	if raw == "" {
		return nil, false, nil
	}
	return []byte(raw), true, nil
}
