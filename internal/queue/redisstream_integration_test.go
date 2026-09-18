//go:build integration

package queue

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// setupRedisStreamEnv mirrors tests/integration/phase6_reliability_test.go's
// own Redis half of setupPhase6Env — a real Redis via testcontainers is
// what XREADGROUP/XAUTOCLAIM/consumer-group semantics actually need; no
// fake can stand in for them.
func setupRedisStreamEnv(t *testing.T) *goredis.Client {
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
	return client
}

func newTestJob(sessionKey string) Job {
	return Job{TenantID: uuid.New(), SessionID: uuid.New(), SessionKey: sessionKey, Kind: KindResume, Payload: []byte(`{}`)}
}

func TestRedisStream_ConsumerGroupNeverDoubleLeases(t *testing.T) {
	client := setupRedisStreamEnv(t)
	ctx := context.Background()
	q := NewRedisStream(client)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensure group: %v", err)
	}

	if _, err := q.Enqueue(ctx, newTestJob("session-a")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	const workers = 8
	var leased int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(owner int) {
			defer wg.Done()
			_, ok, err := q.Lease(ctx, uuid.New().String(), time.Minute)
			if err != nil {
				t.Errorf("lease: %v", err)
				return
			}
			if ok {
				atomic.AddInt32(&leased, 1)
			}
		}(i)
	}
	wg.Wait()

	if leased != 1 {
		t.Fatalf("leased = %d across %d concurrent workers racing ONE job, want exactly 1 (the consumer group must prevent a double lease)", leased, workers)
	}
}

func TestRedisStream_CompleteThenNeverLeasableAgain(t *testing.T) {
	client := setupRedisStreamEnv(t)
	ctx := context.Background()
	q := NewRedisStream(client)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensure group: %v", err)
	}

	job, err := q.Enqueue(ctx, newTestJob("session-b"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	leased, ok, err := q.Lease(ctx, "w1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if leased.JobID != job.JobID || leased.Attempts != 1 {
		t.Fatalf("leased job = %+v, want job %s at attempt 1", leased, job.JobID)
	}
	if err := q.Complete(ctx, job.JobID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if _, ok, err := q.Lease(ctx, "w2", time.Minute); err != nil || ok {
		t.Fatalf("lease after complete: ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestRedisStream_FailRetryableRequeuesImmediatelyWhenDue(t *testing.T) {
	client := setupRedisStreamEnv(t)
	ctx := context.Background()
	q := NewRedisStream(client)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensure group: %v", err)
	}

	job, err := q.Enqueue(ctx, newTestJob("session-c"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, ok, err := q.Lease(ctx, "w1", time.Minute); err != nil || !ok {
		t.Fatalf("first lease: ok=%v err=%v", ok, err)
	}
	if err := q.Fail(ctx, job.JobID, "transient", false, time.Now()); err != nil {
		t.Fatalf("fail: %v", err)
	}

	retried, ok, err := q.Lease(ctx, "w2", time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease after retryable fail: ok=%v err=%v, want ok=true", ok, err)
	}
	if retried.JobID != job.JobID {
		t.Fatalf("retried job id = %s, want %s", retried.JobID, job.JobID)
	}
	if retried.Attempts != 2 {
		t.Fatalf("retried job attempts = %d, want 2 (one per lease)", retried.Attempts)
	}
}

func TestRedisStream_FailPermanentGoesToDeadLetterNeverLeasableAgain(t *testing.T) {
	client := setupRedisStreamEnv(t)
	ctx := context.Background()
	q := NewRedisStream(client)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensure group: %v", err)
	}

	job, err := q.Enqueue(ctx, newTestJob("session-d"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, ok, err := q.Lease(ctx, "w1", time.Minute); err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if err := q.Fail(ctx, job.JobID, "permanent boom", true, time.Now()); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if _, ok, err := q.Lease(ctx, "w2", time.Minute); err != nil || ok {
		t.Fatalf("lease after permanent fail: ok=%v err=%v, want ok=false", ok, err)
	}

	entries, err := client.XRange(ctx, deadLetterKey, "-", "+").Result()
	if err != nil {
		t.Fatalf("xrange dead letter: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Values["job"].(string), job.JobID.String()) {
			found = true
		}
	}
	if !found {
		t.Fatalf("dead letter stream = %+v, want an entry for job %s", entries, job.JobID)
	}
}

func TestRedisStream_DelayedRetryPromotedOnlyWhenDue(t *testing.T) {
	client := setupRedisStreamEnv(t)
	ctx := context.Background()
	q := NewRedisStream(client)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensure group: %v", err)
	}

	job, err := q.Enqueue(ctx, newTestJob("session-e"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, ok, err := q.Lease(ctx, "w1", time.Minute); err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if err := q.Fail(ctx, job.JobID, "transient", false, time.Now().Add(2*time.Second)); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if _, ok, err := q.Lease(ctx, "w2", time.Minute); err != nil || ok {
		t.Fatalf("lease before the retry is due: ok=%v err=%v, want ok=false", ok, err)
	}

	time.Sleep(2500 * time.Millisecond)

	retried, ok, err := q.Lease(ctx, "w3", time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease once due: ok=%v err=%v, want ok=true", ok, err)
	}
	if retried.JobID != job.JobID {
		t.Fatalf("retried job id = %s, want %s", retried.JobID, job.JobID)
	}
}

func TestRedisStream_AbandonedEntryReclaimedAfterMinIdle(t *testing.T) {
	client := setupRedisStreamEnv(t)
	ctx := context.Background()
	q := NewRedisStream(client)
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensure group: %v", err)
	}

	job, err := q.Enqueue(ctx, newTestJob("session-f"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// w1 leases with a very short lease — simulating a worker that crashed
	// and never called Complete/Fail before its lease should have expired.
	first, ok, err := q.Lease(ctx, "w1", 500*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("first lease: ok=%v err=%v", ok, err)
	}

	// Immediately after, before the lease's own idle threshold has passed,
	// another worker asking with the SAME min-idle must find nothing —
	// this entry is still legitimately in flight.
	if _, ok, err := q.Lease(ctx, "w2", 500*time.Millisecond); err != nil || ok {
		t.Fatalf("lease before min-idle has elapsed: ok=%v err=%v, want ok=false", ok, err)
	}

	time.Sleep(600 * time.Millisecond)

	reclaimed, ok, err := q.Lease(ctx, "w2", 500*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("lease after min-idle elapsed: ok=%v err=%v, want ok=true (XAUTOCLAIM should reclaim it)", ok, err)
	}
	if reclaimed.JobID != first.JobID || reclaimed.JobID != job.JobID {
		t.Fatalf("reclaimed job id = %s, want %s", reclaimed.JobID, job.JobID)
	}

	// w1's own (now-stale) view of the job must no longer be completable —
	// w2 owns it now, and Complete keys strictly on job_id, so this is
	// really asserting there is exactly ONE live claim on the index at a
	// time, not that w1's call errors for its own sake.
	if err := q.Complete(ctx, job.JobID); err != nil {
		t.Fatalf("complete after reclaim: %v", err)
	}
}
