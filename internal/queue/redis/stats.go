package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/truongpx396/nexus-agent-demo/internal/queue"
)

var _ queue.Stater = (*Streams)(nil)

// Stats reports the queue's depth in one round trip. A leased job is one delivered
// to a consumer and not yet acknowledged, so Queued is what is on the stream
// beyond those.
func (r *Streams) Stats(ctx context.Context) (queue.Stats, error) {
	pipe := r.client.Pipeline()
	length := pipe.XLen(ctx, streamKey)
	pending := pipe.XPending(ctx, streamKey, groupName)
	delayed := pipe.ZCard(ctx, delayedKey)
	dead := pipe.XLen(ctx, deadKey)
	oldest := pipe.XRangeN(ctx, streamKey, "-", "+", 1)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
		return queue.Stats{}, fmt.Errorf("queue: read depth: %w", err)
	}

	st := queue.Stats{Leased: pending.Val().Count, Delayed: delayed.Val(), Dead: dead.Val()}
	st.Queued = max(length.Val()-st.Leased, 0)
	if entries := oldest.Val(); len(entries) == 1 {
		st.OldestAge = ageOfEntry(entries[0].ID, time.Now())
	}
	return st, nil
}

// ageOfEntry is how old a stream entry is, from the millisecond timestamp its id
// starts with ("1700000000000-0").
func ageOfEntry(id string, now time.Time) time.Duration {
	ms, _, _ := strings.Cut(id, "-")
	n, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return 0
	}
	return max(now.Sub(time.UnixMilli(n)), 0)
}
