//go:build integration

package queue

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// newTestQueue starts a throwaway Redis (the consumer-group, XAUTOCLAIM and
// XDEL semantics under test only exist in a real one) and returns the Port
// plus the raw client so a test can look at the stream itself.
func newTestQueue(t *testing.T) (*RedisStreams, *goredis.Client) {
	t.Helper()
	ctx := context.Background()

	redisC, err := tcredis.Run(ctx, "redis:7")
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() { _ = redisC.Terminate(ctx) })

	connStr, err := redisC.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis connection string: %v", err)
	}
	opts, err := goredis.ParseURL(connStr)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	client := goredis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })

	q, err := NewRedisStreams(ctx, client)
	if err != nil {
		t.Fatalf("new redis streams: %v", err)
	}
	return q, client
}

func newTestJob() Job {
	id := uuid.New()
	return Job{TenantID: uuid.New(), SessionID: id, SessionKey: id.String(), Kind: KindResume}
}

func streamLen(t *testing.T, client *goredis.Client, key string) int64 {
	t.Helper()
	n, err := client.XLen(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("xlen %s: %v", key, err)
	}
	return n
}

func pendingCount(t *testing.T, client *goredis.Client) int64 {
	t.Helper()
	p, err := client.XPending(context.Background(), streamKey, groupName).Result()
	if err != nil {
		t.Fatalf("xpending: %v", err)
	}
	return p.Count
}

// leaseWithin polls Lease until it yields a job or the deadline passes — a
// delayed retry becomes leasable on a wall-clock schedule, not on demand.
func leaseWithin(t *testing.T, q *RedisStreams, owner string, d time.Duration) (Job, bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		job, ok, err := q.Lease(context.Background(), owner, time.Minute)
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if ok || time.Now().After(deadline) {
			return job, ok
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// XACK alone removes an entry from the pending list but leaves it in the
// stream, so without XDEL every completed job would sit in
// nexus:queue:jobs forever.
func TestRedisStreams_CompleteDeletesTheStreamEntry(t *testing.T) {
	q, client := newTestQueue(t)
	ctx := context.Background()

	job, err := q.Enqueue(ctx, newTestJob())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got := streamLen(t, client, streamKey); got != 1 {
		t.Fatalf("stream length after enqueue = %d, want 1", got)
	}
	if _, ok, err := q.Lease(ctx, "w1", time.Minute); err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if err := q.Complete(ctx, job.JobID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if got := streamLen(t, client, streamKey); got != 0 {
		t.Fatalf("stream length after complete = %d, want 0 (a completed job must not stay in the stream)", got)
	}
	if got := pendingCount(t, client); got != 0 {
		t.Fatalf("pending entries after complete = %d, want 0", got)
	}
}

func TestRedisStreams_FailRetryableRetiresOldEntryAndRequeues(t *testing.T) {
	q, client := newTestQueue(t)
	ctx := context.Background()

	job, err := q.Enqueue(ctx, newTestJob())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, ok, err := q.Lease(ctx, "w1", time.Minute); err != nil || !ok {
		t.Fatalf("first lease: ok=%v err=%v", ok, err)
	}
	if err := q.Fail(ctx, job.JobID, "transient", false, time.Now()); err != nil {
		t.Fatalf("fail: %v", err)
	}

	// Exactly one entry: the retry. The failed delivery's entry is gone.
	if got := streamLen(t, client, streamKey); got != 1 {
		t.Fatalf("stream length after retryable fail = %d, want 1 (old entry deleted, retry added)", got)
	}
	retried, ok, err := q.Lease(ctx, "w2", time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease retry: ok=%v err=%v", ok, err)
	}
	if retried.JobID != job.JobID || retried.Attempts != 2 {
		t.Fatalf("retry = job %s attempt %d, want job %s attempt 2", retried.JobID, retried.Attempts, job.JobID)
	}
	if err := q.Complete(ctx, job.JobID); err != nil {
		t.Fatalf("complete retry: %v", err)
	}
	if got := streamLen(t, client, streamKey); got != 0 {
		t.Fatalf("stream length after retry completes = %d, want 0", got)
	}
}

func TestRedisStreams_DelayedRetryIsHeldUntilDue(t *testing.T) {
	q, client := newTestQueue(t)
	ctx := context.Background()

	job, err := q.Enqueue(ctx, newTestJob())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, ok, err := q.Lease(ctx, "w1", time.Minute); err != nil || !ok {
		t.Fatalf("first lease: ok=%v err=%v", ok, err)
	}
	retryAt := time.Now().Add(700 * time.Millisecond)
	if err := q.Fail(ctx, job.JobID, "transient", false, retryAt); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if got := streamLen(t, client, streamKey); got != 0 {
		t.Fatalf("stream length while the retry is delayed = %d, want 0 (it lives in the delay buffer)", got)
	}
	if _, ok, err := q.Lease(ctx, "w2", time.Minute); err != nil || ok {
		t.Fatalf("lease before the retry is due: ok=%v err=%v, want ok=false", ok, err)
	}

	retried, ok := leaseWithin(t, q, "w3", 5*time.Second)
	if !ok {
		t.Fatal("delayed retry was never promoted")
	}
	if time.Now().Before(retryAt) {
		t.Fatalf("retry leased before its retry-at time")
	}
	if retried.JobID != job.JobID {
		t.Fatalf("retried job = %s, want %s", retried.JobID, job.JobID)
	}
}

func TestRedisStreams_PermanentFailureIsDeadLetteredAndRemovedFromStream(t *testing.T) {
	q, client := newTestQueue(t)
	ctx := context.Background()

	job, err := q.Enqueue(ctx, newTestJob())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, ok, err := q.Lease(ctx, "w1", time.Minute); err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if err := q.Fail(ctx, job.JobID, "boom", true, time.Now()); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if got := streamLen(t, client, streamKey); got != 0 {
		t.Fatalf("stream length after permanent fail = %d, want 0", got)
	}
	if _, ok, err := q.Lease(ctx, "w2", time.Minute); err != nil || ok {
		t.Fatalf("lease after permanent fail: ok=%v err=%v, want ok=false", ok, err)
	}
	dead, err := client.XRange(ctx, deadKey, "-", "+").Result()
	if err != nil {
		t.Fatalf("xrange dead: %v", err)
	}
	if len(dead) != 1 || dead[0].Values["job_id"] != job.JobID.String() || dead[0].Values["last_error"] != "boom" {
		t.Fatalf("dead-letter stream = %+v, want one entry for job %s with last_error=boom", dead, job.JobID)
	}
}

func TestRedisStreams_DeadLetterStreamIsCapped(t *testing.T) {
	q, client := newTestQueue(t)
	ctx := context.Background()
	q.deadMaxLen = 3

	var last uuid.UUID
	for i := 0; i < 6; i++ {
		job, err := q.Enqueue(ctx, newTestJob())
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		if _, ok, err := q.Lease(ctx, "w1", time.Minute); err != nil || !ok {
			t.Fatalf("lease %d: ok=%v err=%v", i, ok, err)
		}
		if err := q.Fail(ctx, job.JobID, "boom", true, time.Now()); err != nil {
			t.Fatalf("fail %d: %v", i, err)
		}
		last = job.JobID
	}

	if got := streamLen(t, client, deadKey); got != 3 {
		t.Fatalf("dead-letter length = %d, want 3 (capped)", got)
	}
	newest, err := client.XRevRangeN(ctx, deadKey, "+", "-", 1).Result()
	if err != nil || len(newest) != 1 || newest[0].Values["job_id"] != last.String() {
		t.Fatalf("newest dead-letter entry = %+v err=%v, want the most recent failure %s (the cap must drop the oldest)", newest, err, last)
	}
}

// A malformed entry used to stay pending: XAutoClaim handed it back every
// leaseFor and Lease failed on it each time, forever.
func TestRedisStreams_UnparseableEntryIsDeadLetteredNotRetriedForever(t *testing.T) {
	q, client := newTestQueue(t)
	ctx := context.Background()

	if err := client.XAdd(ctx, &goredis.XAddArgs{Stream: streamKey, Values: map[string]interface{}{"job_id": "not-a-uuid"}}).Err(); err != nil {
		t.Fatalf("xadd malformed entry: %v", err)
	}
	good, err := q.Enqueue(ctx, newTestJob())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// The malformed entry is first in line: Lease reports it, once.
	if _, _, err := q.Lease(ctx, "w1", time.Minute); err == nil {
		t.Fatal("lease of a malformed entry returned no error, want one the worker can log")
	}
	if got := pendingCount(t, client); got != 0 {
		t.Fatalf("pending entries after the malformed lease = %d, want 0 (it must not stay claimable)", got)
	}
	dead, err := client.XRange(ctx, deadKey, "-", "+").Result()
	if err != nil || len(dead) != 1 || dead[0].Values["job_id"] != "not-a-uuid" {
		t.Fatalf("dead-letter stream = %+v err=%v, want the malformed entry preserved for inspection", dead, err)
	}

	// The queue is not wedged: the next lease serves the good job.
	got, ok, err := q.Lease(ctx, "w1", time.Minute)
	if err != nil || !ok || got.JobID != good.JobID {
		t.Fatalf("lease after the malformed entry: job=%v ok=%v err=%v, want job %s", got.JobID, ok, err, good.JobID)
	}
}
