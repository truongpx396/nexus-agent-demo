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

// Redis key/stream names this adapter owns. One stream carries every Kind
// (mirroring queue_jobs being one table), one consumer group serves every
// Worker (WorkerConfig.Owner becomes the group's consumer name — already
// unique: "nexusd-worker-<pid>-<i>").
const (
	streamKey      = "nexus:queue:jobs"
	groupName      = "nexus:queue:workers"
	delayedKey     = "nexus:queue:delayed"  // ZSET: member = job JSON, score = retry-at unix ms
	deadKey        = "nexus:queue:dead"     // plain stream, no group — permanent failures, for operator inspection
	entryKeyPrefix = "nexus:queue:entry:"   // job_id -> {entry_id, job JSON}, HASH, set only while leased
	entryKeyTTL    = 24 * time.Hour         // safety net so a forgotten claim can't leak forever
	leaseBlock     = 200 * time.Millisecond // shorter than Worker's own default PollEvery (500ms) so Lease never overruns the next tick
	promoteBatch   = 10

	// defaultDeadMaxLen caps the dead-letter stream (XADD MAXLEN, exact). Failed
	// jobs are only ever inspected by an operator, so the newest N is the
	// useful window; without a cap a crash-looping job class grows it
	// forever. The live stream needs no cap: Complete/Fail XDEL their own
	// entry, so it holds only what is still queued or in flight — a MAXLEN
	// there could silently drop a queued job under backlog.
	defaultDeadMaxLen = 10_000
)

// RedisStreams implements Port against a Redis Stream + consumer group —
// the demo's Redis-backed successor to the retired Postgres SKIP LOCKED
// adapter (NATS JetStream remains the deferred, real-production option
// this package's own doc comment names). XAutoClaim's reclaim of an
// abandoned consumer-group entry is the direct analogue of SKIP LOCKED's
// row-lock atomicity: it is a single atomic Redis command, so two workers
// racing it can never both walk away owning the same job.
//
// Unlike the retired Postgres adapter — whose lease_expires_at was written
// on every Lease and read nowhere, so an abandoned lease was never actually
// reclaimed, only ever recovered a process-boot-later by cmd/nexusd's own
// orphaned-session sweep — this adapter's reclaim is real: Lease itself
// tries XAutoClaim before reading anything new.
type RedisStreams struct {
	client     *redis.Client
	deadMaxLen int64
}

// NewRedisStreams creates the consumer group (idempotent — a group that
// already exists is not an error) and returns a ready-to-use Port.
func NewRedisStreams(ctx context.Context, client *redis.Client) (*RedisStreams, error) {
	if err := client.XGroupCreateMkStream(ctx, streamKey, groupName, "$").Err(); err != nil &&
		!strings.Contains(err.Error(), "BUSYGROUP") {
		return nil, fmt.Errorf("queue: create consumer group: %w", err)
	}
	return &RedisStreams{client: client, deadMaxLen: defaultDeadMaxLen}, nil
}

// CheckReady confirms this process's own consumer group actually exists —
// cmd/nexusd's /readyz calls this alongside its existing bare Redis ping
// (README task 13.2's own "Postgres + Redis + signerd socket reachable"
// intent already implies "the thing I depend on is actually there," not
// just "Redis answers a PING").
func CheckReady(ctx context.Context, client *redis.Client) error {
	if err := client.XInfoGroups(ctx, streamKey).Err(); err != nil {
		return fmt.Errorf("queue: consumer group %q on stream %q not ready: %w", groupName, streamKey, err)
	}
	return nil
}

// jobFields is the entry shape written to the stream/dead-letter stream —
// ids and control fields only, never plaintext, the same discipline
// queue_jobs.payload's own retired doc comment described.
func jobFields(job Job) map[string]interface{} {
	return map[string]interface{}{
		"job_id":      job.JobID.String(),
		"tenant_id":   job.TenantID.String(),
		"session_id":  job.SessionID.String(),
		"session_key": job.SessionKey,
		"kind":        string(job.Kind),
		"attempts":    strconv.Itoa(job.Attempts),
	}
}

func parseJobFields(values map[string]interface{}) (Job, error) {
	str := func(k string) string {
		v, _ := values[k].(string)
		return v
	}
	var job Job
	var err error
	if job.JobID, err = uuid.Parse(str("job_id")); err != nil {
		return Job{}, fmt.Errorf("parse job_id: %w", err)
	}
	if job.TenantID, err = uuid.Parse(str("tenant_id")); err != nil {
		return Job{}, fmt.Errorf("parse tenant_id: %w", err)
	}
	if job.SessionID, err = uuid.Parse(str("session_id")); err != nil {
		return Job{}, fmt.Errorf("parse session_id: %w", err)
	}
	job.SessionKey = str("session_key")
	job.Kind = Kind(str("kind"))
	job.Attempts, _ = strconv.Atoi(str("attempts"))
	return job, nil
}

func (r *RedisStreams) Enqueue(ctx context.Context, job Job) (Job, error) {
	if job.JobID == uuid.Nil {
		job.JobID = uuid.New()
	}
	job.Status = StatusPending
	job.CreatedAt = time.Now()

	if !job.AvailableAt.IsZero() && job.AvailableAt.After(time.Now()) {
		// A caller-specified future AvailableAt (Port.Enqueue's own doc
		// comment: "a deliberately delayed retry") goes straight into the
		// delay buffer, exactly like Fail's own retryAt-in-the-future path.
		if err := r.pushDelayed(ctx, job, job.AvailableAt); err != nil {
			return Job{}, fmt.Errorf("queue: enqueue delayed job %s: %w", job.JobID, err)
		}
		return job, nil
	}

	if _, err := r.client.XAdd(ctx, &redis.XAddArgs{Stream: streamKey, Values: jobFields(job)}).Result(); err != nil {
		return Job{}, fmt.Errorf("queue: enqueue job %s: %w", job.JobID, err)
	}
	return job, nil
}

// Lease tries, in order: (1) promote any delayed retries whose backoff has
// elapsed onto the live stream, (2) XAutoClaim one entry idle longer than
// leaseFor (a previous claimant that never Completed/Failed it — a crashed
// worker), (3) XReadGroup one brand-new entry, blocking briefly so a quiet
// queue doesn't spin. ok is false when neither (2) nor (3) produced
// anything.
func (r *RedisStreams) Lease(ctx context.Context, owner string, leaseFor time.Duration) (Job, bool, error) {
	if err := r.promoteDelayed(ctx); err != nil {
		return Job{}, false, fmt.Errorf("queue: lease: %w", err)
	}

	claimed, _, err := r.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: streamKey, Group: groupName, Consumer: owner,
		MinIdle: leaseFor, Start: "0-0", Count: 1,
	}).Result()
	if err != nil {
		return Job{}, false, fmt.Errorf("queue: lease: xautoclaim: %w", err)
	}
	if len(claimed) > 0 {
		job, err := r.claim(ctx, owner, claimed[0])
		if err != nil {
			return Job{}, false, fmt.Errorf("queue: lease: %w", err)
		}
		return job, true, nil
	}

	streams, err := r.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: groupName, Consumer: owner, Streams: []string{streamKey, ">"},
		Count: 1, Block: leaseBlock,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return Job{}, false, nil // nothing new within the block window — same "nothing leasable" contract Postgres's Lease gives
		}
		return Job{}, false, fmt.Errorf("queue: lease: xreadgroup: %w", err)
	}
	if len(streams) == 0 || len(streams[0].Messages) == 0 {
		return Job{}, false, nil
	}
	job, err := r.claim(ctx, owner, streams[0].Messages[0])
	if err != nil {
		return Job{}, false, fmt.Errorf("queue: lease: %w", err)
	}
	return job, true, nil
}

// claim finishes what XAutoClaim/XReadGroup started: parse the delivered
// entry, bump Attempts (this delivery IS the new attempt, whether it's
// brand-new or a reclaim), and record {entry_id, job} in a Hash so a later
// Complete/Fail call — which the Port interface only gives a bare jobID,
// never the physical stream entry id — can find its way back to both.
//
// A stream entry's own fields are immutable once XAdd'd, so an XAutoClaim
// reclaim (same entry, same "attempts" field value it was written with)
// can't see what a PRIOR claim already bumped it to in memory — that prior
// claim's own {entry_id, job} Hash is still sitting there precisely
// because nobody ever called Complete/Fail to delete it (the abandoned-
// lease case XAutoClaim exists to reclaim), so it — not the stream
// entry's own stale field — is the accurate running Attempts count when
// present.
func (r *RedisStreams) claim(ctx context.Context, owner string, msg redis.XMessage) (Job, error) {
	job, err := parseJobFields(msg.Values)
	if err != nil {
		if dlErr := r.discardUnparseable(ctx, msg, err); dlErr != nil {
			return Job{}, fmt.Errorf("parse claimed entry %s: %w (and could not dead-letter it: %v)", msg.ID, err, dlErr)
		}
		return Job{}, fmt.Errorf("parse claimed entry %s (dead-lettered): %w", msg.ID, err)
	}
	if _, prior, err := r.claimedEntry(ctx, job.JobID); err == nil {
		job.Attempts = prior.Attempts
	}
	job.Attempts++
	job.Status = StatusLeased
	job.LeaseOwner = owner

	raw, err := json.Marshal(job)
	if err != nil {
		return Job{}, fmt.Errorf("marshal claimed job %s: %w", job.JobID, err)
	}
	key := entryKeyPrefix + job.JobID.String()
	pipe := r.client.TxPipeline()
	pipe.HSet(ctx, key, "entry_id", msg.ID, "job", raw)
	pipe.Expire(ctx, key, entryKeyTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return Job{}, fmt.Errorf("record claim for job %s: %w", job.JobID, err)
	}
	return job, nil
}

// promoteDelayed moves every delay-buffer member whose retry-at has
// elapsed back onto the live stream. ZRem's return value is what keeps two
// workers racing this in the same tick from double-promoting: both may see
// the same due member from ZRANGE BYSCORE's read below, but Redis
// serializes the ZRem calls that follow, so only the one that actually
// removes it (result 1, not 0) is allowed to XAdd it back — the other sees
// 0 and skips, no Lua script needed for that ordering to be race-free.
func (r *RedisStreams) promoteDelayed(ctx context.Context) error {
	max := strconv.FormatInt(time.Now().UnixMilli(), 10)
	// ZRANGE ... BYSCORE (Redis 6.2+, the deployment and tests run redis:7)
	// replaces the deprecated ZRANGEBYSCORE; same range, same LIMIT 0 N.
	due, err := r.client.ZRangeArgs(ctx, redis.ZRangeArgs{Key: delayedKey, Start: "-inf", Stop: max, ByScore: true, Count: promoteBatch}).Result()
	if err != nil {
		return fmt.Errorf("list due delayed jobs: %w", err)
	}
	for _, raw := range due {
		removed, err := r.client.ZRem(ctx, delayedKey, raw).Result()
		if err != nil {
			return fmt.Errorf("promote delayed job: zrem: %w", err)
		}
		if removed == 0 {
			continue // another worker's concurrent Lease call already promoted this one
		}
		var job Job
		if err := json.Unmarshal([]byte(raw), &job); err != nil {
			continue // a malformed delay-buffer entry can never be promoted again — drop it rather than block every future Lease on it
		}
		if _, err := r.client.XAdd(ctx, &redis.XAddArgs{Stream: streamKey, Values: jobFields(job)}).Result(); err != nil {
			return fmt.Errorf("promote delayed job %s: xadd: %w", job.JobID, err)
		}
	}
	return nil
}

func (r *RedisStreams) pushDelayed(ctx context.Context, job Job, at time.Time) error {
	raw, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal delayed job %s: %w", job.JobID, err)
	}
	if err := r.client.ZAdd(ctx, delayedKey, redis.Z{Score: float64(at.UnixMilli()), Member: raw}).Err(); err != nil {
		return fmt.Errorf("push delayed job %s: %w", job.JobID, err)
	}
	return nil
}

// claimedEntry looks up the {entry_id, job} a prior Lease recorded for
// jobID — the only way Complete/Fail (which the Port interface hands only
// a bare uuid.UUID) can find the physical stream entry to XAck, or the
// job's own fields to requeue/dead-letter.
func (r *RedisStreams) claimedEntry(ctx context.Context, jobID uuid.UUID) (entryID string, job Job, err error) {
	vals, err := r.client.HGetAll(ctx, entryKeyPrefix+jobID.String()).Result()
	if err != nil {
		return "", Job{}, fmt.Errorf("look up claim for job %s: %w", jobID, err)
	}
	entryID, ok := vals["entry_id"]
	if !ok {
		return "", Job{}, fmt.Errorf("complete/fail job %s: no such leased job (claim not found or expired)", jobID)
	}
	if err := json.Unmarshal([]byte(vals["job"]), &job); err != nil {
		return "", Job{}, fmt.Errorf("unmarshal claimed job %s: %w", jobID, err)
	}
	return entryID, job, nil
}

func (r *RedisStreams) Complete(ctx context.Context, jobID uuid.UUID) error {
	entryID, _, err := r.claimedEntry(ctx, jobID)
	if err != nil {
		return fmt.Errorf("queue: complete job %s: %w", jobID, err)
	}
	if err := r.retire(ctx, jobID, entryID); err != nil {
		return fmt.Errorf("queue: complete job %s: %w", jobID, err)
	}
	return nil
}

// retire is the end of one delivery: XACK drops the entry from the group's
// pending list, XDEL removes it from the stream itself (XACK alone leaves
// the entry in the stream forever), and the claim record goes with it — all
// in one transaction, so a crash can't leave a half-retired entry.
func (r *RedisStreams) retire(ctx context.Context, jobID uuid.UUID, entryID string) error {
	pipe := r.client.TxPipeline()
	pipe.XAck(ctx, streamKey, groupName, entryID)
	pipe.XDel(ctx, streamKey, entryID)
	pipe.Del(ctx, entryKeyPrefix+jobID.String())
	_, err := pipe.Exec(ctx)
	return err
}

// discardUnparseable dead-letters an entry whose fields can't be parsed and
// retires it. Left pending, XAutoClaim would hand it back every leaseFor and
// Lease would fail on it forever. The raw fields are kept on the dead-letter
// stream (ids and control fields only, same as any job entry) for an
// operator to inspect.
func (r *RedisStreams) discardUnparseable(ctx context.Context, msg redis.XMessage, cause error) error {
	dead := make(map[string]interface{}, len(msg.Values)+2)
	for k, v := range msg.Values {
		dead[k] = v
	}
	dead["entry_id"] = msg.ID
	dead["last_error"] = "unparseable entry: " + cause.Error()

	pipe := r.client.TxPipeline()
	pipe.XAdd(ctx, &redis.XAddArgs{Stream: deadKey, MaxLen: r.deadMaxLen, Values: dead})
	pipe.XAck(ctx, streamKey, groupName, msg.ID)
	pipe.XDel(ctx, streamKey, msg.ID)
	_, err := pipe.Exec(ctx)
	return err
}

// Fail retires the job's original entry either way (acked and deleted, so
// it is never left in the PEL to be reclaimed AGAIN by XAutoClaim — retry
// scheduling from here on is this method's job, not idle-timeout's) and
// then, depending on
// permanent/retryAt: dead-letters it (nexus:queue:dead, a plain
// inspectable stream — the operator-facing view queue_jobs.status='failed'
// rows used to give for free), re-adds it immediately (retryAt already
// elapsed), or parks it in the delay buffer for promoteDelayed to pick up
// later.
func (r *RedisStreams) Fail(ctx context.Context, jobID uuid.UUID, reason string, permanent bool, retryAt time.Time) error {
	entryID, job, err := r.claimedEntry(ctx, jobID)
	if err != nil {
		return fmt.Errorf("queue: fail job %s: %w", jobID, err)
	}

	if err := r.retire(ctx, jobID, entryID); err != nil {
		return fmt.Errorf("queue: fail job %s: retire original entry: %w", jobID, err)
	}

	if permanent {
		dead := jobFields(job)
		dead["last_error"] = reason
		if err := r.client.XAdd(ctx, &redis.XAddArgs{Stream: deadKey, MaxLen: r.deadMaxLen, Values: dead}).Err(); err != nil {
			return fmt.Errorf("queue: fail job %s: dead-letter: %w", jobID, err)
		}
		return nil
	}

	if !retryAt.After(time.Now()) {
		if _, err := r.client.XAdd(ctx, &redis.XAddArgs{Stream: streamKey, Values: jobFields(job)}).Result(); err != nil {
			return fmt.Errorf("queue: fail job %s: requeue: %w", jobID, err)
		}
		return nil
	}
	if err := r.pushDelayed(ctx, job, retryAt); err != nil {
		return fmt.Errorf("queue: fail job %s: %w", jobID, err)
	}
	return nil
}
