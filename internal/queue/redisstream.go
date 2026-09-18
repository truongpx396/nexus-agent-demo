package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RedisStream implements Port over a Redis Stream + consumer group
// (docs/build-phases.md Phase 18) — the default queue backend as of this
// phase, replacing Postgres SKIP LOCKED (postgres.go, kept as-is: still an
// adapter behind Port, selectable via NEXUS_QUEUE_BACKEND=postgres in
// cmd/nexusd/serve.go, never deleted). XREADGROUP's own per-consumer
// delivery bookkeeping is what SKIP LOCKED's FOR UPDATE gave the Postgres
// adapter for free: two workers in the SAME consumer group can never both
// read the same entry. A worker that dies mid-job leaves its entry on the
// group's pending-entries list, where ReclaimAbandoned's own logic inside
// Lease (XAUTOCLAIM, keyed on the caller's own leaseFor as the minimum
// idle time) picks it back up on a LATER Lease call from any live
// consumer — a continuous mechanism that replaces this demo's prior
// "sweep every session with status='running' at THIS process's own
// startup" (cmd/nexusd/background.go's recoverOrphanedSessions, which
// still runs unchanged: it only decides WHAT to enqueue, never how the
// queue backend leases/reclaims it).
//
// AvailableAt (a delayed retry after a transient failure,
// internal/reliability's own backoff) has no native Streams equivalent
// (unlike SQS's per-message visibility delay): Enqueue/Fail with a future
// AvailableAt/retryAt scores the job into a small Redis sorted set
// (delayedKey) instead of the stream directly; every Lease call first
// promotes anything whose score has passed back onto the stream
// (promoteDue) — the same "a sorted set as a delay buffer in front of a
// stream" shape Redis's own Streams documentation recommends for exactly
// this gap. Popping due entries is a single Lua script (ZRANGEBYSCORE +
// ZREM), the same atomic-compare-and-act discipline lock.go's own
// luaCompareAndDelete/luaCompareAndExtend already establish in this
// package, so two workers racing the same due entry can never both
// promote (and thus double-process) it.
//
// Stream entries are immutable, so a retried job (Fail with permanent=
// false) can't be updated in place: the old entry is Ack'd+deleted and a
// brand-new one is XAdd'ed carrying the incremented attempt count — which
// means Complete/Fail need the job's own field values, not just which
// entry to acknowledge. leasedIndexKey (a Redis hash, job_id -> a small
// JSON snapshot written at Lease time) is what makes that possible without
// widening the Port interface itself: Postgres's UPDATE ... WHERE job_id=
// $1 can mutate a row in place because the row IS the job; Streams can't,
// so this index is the honest replacement, not a shortcut.
//
// A permanently-failed job (permanent=true) is moved to deadLetterKey — a
// separate, capped stream an operator can inspect with a plain XRANGE —
// rather than silently discarded, preserving the audit visibility a
// Postgres row left in status='failed' gave for free.
type RedisStream struct {
	client *redis.Client
}

func NewRedisStream(client *redis.Client) *RedisStream {
	return &RedisStream{client: client}
}

const (
	streamKey      = "nexus:queue:jobs"
	deadLetterKey  = "nexus:queue:dead"
	delayedKey     = "nexus:queue:delayed"
	leasedIndexKey = "nexus:queue:leased"
	consumerGroup  = "nexus-workers"

	// streamMaxLen bounds the stream's own approximate length (XADD
	// MAXLEN ~) — Complete/Fail always XDEL their own entry immediately,
	// so under normal operation the stream never approaches this; it only
	// guards against unbounded growth from a pathological run of jobs that
	// are leased but never Complete'd/Fail'd by any consumer (a bug
	// elsewhere, not this adapter's own steady state). A trimmed-but-still
	// -pending entry is a known, accepted Streams caveat at this scale —
	// not reached by any realistic demo workload.
	streamMaxLen = 100_000

	// promoteDueBatch caps how many due delayed jobs one Lease call
	// promotes at once — bounded work per poll tick, matching
	// internal/queue/worker.go's own PollEvery cadence rather than a
	// caller blocking on an unbounded backlog.
	promoteDueBatch = 50
)

// leasedEntry is what leasedIndexKey stores per in-flight job_id — the
// Stream entry id to Ack/Del (EntryID) plus the job's own field values
// (everything Fail needs to re-XAdd a retry, since the old entry is gone
// by the time Fail runs).
type leasedEntry struct {
	EntryID    string          `json:"entry_id"`
	JobID      uuid.UUID       `json:"job_id"`
	TenantID   uuid.UUID       `json:"tenant_id"`
	SessionID  uuid.UUID       `json:"session_id"`
	SessionKey string          `json:"session_key"`
	Kind       Kind            `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	Attempts   int             `json:"attempts"`
	CreatedAt  time.Time       `json:"created_at"`
}

// EnsureGroup creates the consumer group (and the stream itself, via
// MKSTREAM) if neither already exists — idempotent, safe to call on every
// process startup, the same "ensure the durable topology exists" idiom
// store.Migrate already establishes for the Postgres side.
func (q *RedisStream) EnsureGroup(ctx context.Context) error {
	err := q.client.XGroupCreateMkStream(ctx, streamKey, consumerGroup, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("queue: ensure consumer group: %w", err)
	}
	return nil
}

func (q *RedisStream) Enqueue(ctx context.Context, job Job) (Job, error) {
	if job.JobID == uuid.Nil {
		job.JobID = uuid.New()
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now()
	}
	if job.AvailableAt.After(time.Now()) {
		if err := q.scheduleDelayed(ctx, job); err != nil {
			return Job{}, err
		}
		job.Status = StatusPending
		return job, nil
	}
	return q.enqueueNow(ctx, job)
}

func (q *RedisStream) enqueueNow(ctx context.Context, job Job) (Job, error) {
	job.Status = StatusPending
	if _, err := q.client.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		MaxLen: streamMaxLen,
		Approx: true,
		Values: jobFields(job),
	}).Result(); err != nil {
		return Job{}, fmt.Errorf("queue: xadd job %s: %w", job.JobID, err)
	}
	return job, nil
}

func (q *RedisStream) scheduleDelayed(ctx context.Context, job Job) error {
	job.Status = StatusPending
	b, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("queue: marshal delayed job %s: %w", job.JobID, err)
	}
	if err := q.client.ZAdd(ctx, delayedKey, redis.Z{Score: float64(job.AvailableAt.Unix()), Member: string(b)}).Err(); err != nil {
		return fmt.Errorf("queue: schedule delayed job %s: %w", job.JobID, err)
	}
	return nil
}

// luaPopDue atomically pops every member of KEYS[1] scored <= ARGV[1], up
// to ARGV[2] of them — the ZREM happens inside the script, so two workers
// calling promoteDue concurrently can never both pop (and thus both
// re-enqueue) the SAME due entry.
const luaPopDue = `
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, ARGV[2])
for i, member in ipairs(due) do
  redis.call('ZREM', KEYS[1], member)
end
return due
`

func (q *RedisStream) promoteDue(ctx context.Context) error {
	res, err := q.client.Eval(ctx, luaPopDue, []string{delayedKey}, time.Now().Unix(), promoteDueBatch).Result()
	if err != nil {
		return fmt.Errorf("queue: pop due delayed jobs: %w", err)
	}
	due, _ := res.([]interface{})
	for _, m := range due {
		s, _ := m.(string)
		var job Job
		if err := json.Unmarshal([]byte(s), &job); err != nil {
			// A malformed delayed entry must not wedge every future Lease
			// call on it — drop it rather than retry-looping forever on
			// data that will never parse.
			continue
		}
		if _, err := q.enqueueNow(ctx, job); err != nil {
			return fmt.Errorf("queue: promote due delayed job %s: %w", job.JobID, err)
		}
	}
	return nil
}

func (q *RedisStream) Lease(ctx context.Context, owner string, leaseFor time.Duration) (Job, bool, error) {
	if err := q.promoteDue(ctx); err != nil {
		return Job{}, false, err
	}

	// Reclaim first: an entry idle longer than leaseFor means whoever
	// claimed it last (this consumer or another) never Complete'd/Fail'd
	// it within its own lease — the SAME "abandoned" definition Postgres's
	// lease_expires_at encodes, expressed as XAUTOCLAIM's min-idle-time
	// instead of a column comparison.
	claimed, _, err := q.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: streamKey, Group: consumerGroup, MinIdle: leaseFor,
		Start: "0-0", Count: 1, Consumer: owner,
	}).Result()
	if err != nil {
		return Job{}, false, fmt.Errorf("queue: xautoclaim: %w", err)
	}

	var msg redis.XMessage
	var got bool
	if len(claimed) > 0 {
		msg, got = claimed[0], true
	} else {
		streams, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: consumerGroup, Consumer: owner,
			Streams: []string{streamKey, ">"}, Count: 1,
			// Block defaults to 0 (block forever) in go-redis's own arg
			// encoding — any Block >= 0 emits a BLOCK clause, and BLOCK 0
			// means "wait indefinitely" in Redis itself, not "don't wait".
			// This adapter's own contract (Port.Lease "ok is false when
			// nothing is currently leasable") needs the command to return
			// immediately instead — the negative sentinel is what
			// suppresses the BLOCK clause entirely.
			Block: -1,
		}).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return Job{}, false, fmt.Errorf("queue: xreadgroup: %w", err)
		}
		if len(streams) > 0 && len(streams[0].Messages) > 0 {
			msg, got = streams[0].Messages[0], true
		}
	}
	if !got {
		return Job{}, false, nil
	}

	job, err := parseJobFields(msg.Values)
	if err != nil {
		// A malformed entry must not wedge the stream forever — ack/delete
		// it and report nothing leasable this round rather than
		// crash-looping the caller on data that will never parse.
		_ = q.client.XAck(ctx, streamKey, consumerGroup, msg.ID).Err()
		_ = q.client.XDel(ctx, streamKey, msg.ID).Err()
		return Job{}, false, fmt.Errorf("queue: parse leased entry %s: %w", msg.ID, err)
	}
	leaseExpires := time.Now().Add(leaseFor)
	job.Status = StatusLeased
	job.LeaseOwner = owner
	job.LeaseExpiresAt = &leaseExpires
	// Mirrors postgres.go's own "attempts = attempts + 1, RETURNING
	// attempts" — a fresh lease's first attempt reads back as 1. On a
	// RECLAIM (this entry was already delivered once before, to a
	// consumer that never Ack'd it) this undercounts the true delivery
	// count by however many prior deliveries happened, since the stream
	// entry's own "attempts" field is written once at XADD time and never
	// mutated in place — a cosmetic gap in reliability.Classify's own
	// attempt-based backoff/breaker heuristics, never in the exactly-once
	// -lease guarantee itself (XAUTOCLAIM/XREADGROUP's consumer-group
	// bookkeeping is what actually prevents a double lease).
	job.Attempts++

	snap := leasedEntry{
		EntryID: msg.ID, JobID: job.JobID, TenantID: job.TenantID, SessionID: job.SessionID,
		SessionKey: job.SessionKey, Kind: job.Kind, Payload: job.Payload,
		Attempts: job.Attempts, CreatedAt: job.CreatedAt,
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return Job{}, false, fmt.Errorf("queue: marshal leased entry index for job %s: %w", job.JobID, err)
	}
	if err := q.client.HSet(ctx, leasedIndexKey, job.JobID.String(), b).Err(); err != nil {
		return Job{}, false, fmt.Errorf("queue: index leased entry for job %s: %w", job.JobID, err)
	}
	return job, true, nil
}

func (q *RedisStream) Complete(ctx context.Context, jobID uuid.UUID) error {
	snap, err := q.popLeasedEntry(ctx, jobID)
	if err != nil {
		return err
	}
	pipe := q.client.TxPipeline()
	pipe.XAck(ctx, streamKey, consumerGroup, snap.EntryID)
	pipe.XDel(ctx, streamKey, snap.EntryID)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("queue: complete job %s: %w", jobID, err)
	}
	return nil
}

func (q *RedisStream) Fail(ctx context.Context, jobID uuid.UUID, reason string, permanent bool, retryAt time.Time) error {
	snap, err := q.popLeasedEntry(ctx, jobID)
	if err != nil {
		return err
	}
	pipe := q.client.TxPipeline()
	pipe.XAck(ctx, streamKey, consumerGroup, snap.EntryID)
	pipe.XDel(ctx, streamKey, snap.EntryID)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("queue: fail job %s: retire old entry: %w", jobID, err)
	}

	job := Job{
		JobID: snap.JobID, TenantID: snap.TenantID, SessionID: snap.SessionID, SessionKey: snap.SessionKey,
		Kind: snap.Kind, Payload: snap.Payload, Attempts: snap.Attempts, CreatedAt: snap.CreatedAt,
		LastError: reason, AvailableAt: retryAt,
	}
	if permanent {
		return q.sendToDeadLetter(ctx, job, reason)
	}
	if !retryAt.After(time.Now()) {
		_, err := q.enqueueNow(ctx, job)
		return err
	}
	return q.scheduleDelayed(ctx, job)
}

// popLeasedEntry reads and clears jobID's leasedIndexKey entry — shared by
// Complete and Fail, both of which need the same "which entry, and what
// were its own fields" lookup before retiring it.
func (q *RedisStream) popLeasedEntry(ctx context.Context, jobID uuid.UUID) (leasedEntry, error) {
	raw, err := q.client.HGet(ctx, leasedIndexKey, jobID.String()).Result()
	if errors.Is(err, redis.Nil) {
		return leasedEntry{}, fmt.Errorf("queue: no such leased job %s", jobID)
	}
	if err != nil {
		return leasedEntry{}, fmt.Errorf("queue: look up leased entry for job %s: %w", jobID, err)
	}
	var snap leasedEntry
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return leasedEntry{}, fmt.Errorf("queue: decode leased entry for job %s: %w", jobID, err)
	}
	if err := q.client.HDel(ctx, leasedIndexKey, jobID.String()).Err(); err != nil {
		return leasedEntry{}, fmt.Errorf("queue: clear leased entry for job %s: %w", jobID, err)
	}
	return snap, nil
}

func (q *RedisStream) sendToDeadLetter(ctx context.Context, job Job, reason string) error {
	job.Status = StatusFailed
	job.LastError = reason
	b, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("queue: marshal dead-letter job %s: %w", job.JobID, err)
	}
	if err := q.client.XAdd(ctx, &redis.XAddArgs{
		Stream: deadLetterKey,
		MaxLen: streamMaxLen,
		Approx: true,
		Values: map[string]interface{}{"job": string(b), "reason": reason},
	}).Err(); err != nil {
		return fmt.Errorf("queue: send job %s to dead letter: %w", job.JobID, err)
	}
	return nil
}

func jobFields(job Job) map[string]interface{} {
	payload := job.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	return map[string]interface{}{
		"job_id":      job.JobID.String(),
		"tenant_id":   job.TenantID.String(),
		"session_id":  job.SessionID.String(),
		"session_key": job.SessionKey,
		"kind":        string(job.Kind),
		"payload":     string(payload),
		"attempts":    strconv.Itoa(job.Attempts),
		"created_at":  job.CreatedAt.Format(time.RFC3339Nano),
	}
}

func parseJobFields(values map[string]interface{}) (Job, error) {
	str := func(k string) string {
		v, _ := values[k].(string)
		return v
	}
	jobID, err := uuid.Parse(str("job_id"))
	if err != nil {
		return Job{}, fmt.Errorf("parse job_id: %w", err)
	}
	tenantID, err := uuid.Parse(str("tenant_id"))
	if err != nil {
		return Job{}, fmt.Errorf("parse tenant_id: %w", err)
	}
	sessionID, err := uuid.Parse(str("session_id"))
	if err != nil {
		return Job{}, fmt.Errorf("parse session_id: %w", err)
	}
	attempts, _ := strconv.Atoi(str("attempts"))
	createdAt, _ := time.Parse(time.RFC3339Nano, str("created_at"))
	payload := json.RawMessage(str("payload"))
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	return Job{
		JobID: jobID, TenantID: tenantID, SessionID: sessionID,
		SessionKey: str("session_key"), Kind: Kind(str("kind")),
		Payload: payload, Attempts: attempts, CreatedAt: createdAt,
	}, nil
}
