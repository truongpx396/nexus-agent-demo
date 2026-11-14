package runctl

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent/internal/queue"
)

// Orphan is a session some durable record says needs a worker and none has:
// one the database still says is running, or one whose approval was decided
// and never resumed. Kind and Payload say which job to enqueue; an empty Kind
// is a resume.
type Orphan struct {
	TenantID   uuid.UUID
	SessionID  uuid.UUID
	SessionKey string
	Kind       queue.Kind
	Payload    json.RawMessage
}

// Liveness says whether a worker is driving a session right now. The Redis
// session lock (internal/queue/redis) satisfies it.
type Liveness interface {
	Held(ctx context.Context, sessionKey string) (bool, error)
}

// Throttle limits how often one session is re-enqueued by recovery. The Redis
// RecoveryThrottle (internal/queue/redis) satisfies it.
type Throttle interface {
	// Allow reports whether sessionKey may be re-enqueued now, and records that it
	// was if so.
	Allow(ctx context.Context, sessionKey string) (bool, error)
}

// RecoverOrphans enqueues a queue.KindResume job for every candidate that no
// worker is driving, and returns how many. "Running in the database" alone does
// not make a session an orphan once there is more than one process: another
// process's worker may be mid-turn on it, and resuming that would only fail
// noisily at best. So a session whose lock is held is live and is skipped, and
// one whose liveness cannot be read is skipped too (unknown is not orphaned; the
// recheck covers it).
//
// throttle, if not nil, is asked once per session that would be re-enqueued, so a
// session whose job keeps failing is not re-enqueued on every pass of a recurring
// sweep. A session it refuses, or one it cannot answer for, is skipped and counted.
// nil allows every session, which is what the one-off startup passes want.
func RecoverOrphans(ctx context.Context, pass string, candidates []Orphan, live Liveness, port queue.Port, throttle Throttle) (recovered int) {
	var skippedLive, unknown, throttled int
	for _, c := range candidates {
		held, err := live.Held(ctx, c.SessionKey)
		if err != nil {
			log.Warn().Err(err).Any("session_id", c.SessionID).Msg("runctl: cannot tell whether a running session is live; leaving it")
			unknown++
			continue
		}
		if held {
			skippedLive++
			continue
		}
		if throttle != nil {
			allowed, err := throttle.Allow(ctx, c.SessionKey)
			if err != nil {
				log.Warn().Err(err).Any("session_id", c.SessionID).Msg("runctl: cannot tell whether recovery re-enqueued this session recently; leaving it")
			}
			if err != nil || !allowed {
				throttled++
				continue
			}
		}
		kind := c.Kind
		if kind == "" {
			kind = queue.KindResume
		}
		if _, err := port.Enqueue(ctx, queue.Job{TenantID: c.TenantID, SessionID: c.SessionID, SessionKey: c.SessionKey, Kind: kind, Payload: c.Payload}); err != nil {
			log.Error().Err(err).Any("session_id", c.SessionID).Msg("runctl: enqueue resume for orphaned session failed")
			continue
		}
		recovered++
	}
	if recovered > 0 || skippedLive > 0 || unknown > 0 || throttled > 0 {
		log.Info().Str("pass", pass).Int("recovered", recovered).Int("skippedLiveelsewhere", skippedLive).Int("unknown", unknown).Int("throttled", throttled).
			Msg("runctl: orphaned-session sweep")
	}
	return recovered
}

// StartRecovery sweeps for orphaned sessions now, then once more after
// recheckAfter. It is not itself a loop: cmd/nexusd runs the recurring sweep
// (RecoverOrphans with a Throttle, and ListRunning leaving out a session that needs
// a person's claim resolution), so a job lost while every process stays up is
// recovered without a restart. The first sweep runs before it returns, so recovery
// still happens before the workers start. The recheck exists because a worker that died
// mid-run leaves its lock behind until the TTL passes, so the startup sweep
// has to skip that session as if it were live; the recheck, a TTL later, finds
// the lock gone and recovers it.
func StartRecovery(ctx context.Context, list func(context.Context) ([]Orphan, error), live Liveness, port queue.Port, recheckAfter time.Duration) {
	sweep := func(pass string) {
		candidates, err := list(ctx)
		if err != nil {
			log.Error().Err(err).Str("pass", pass).Msg("runctl: list running sessions failed")
			return
		}
		RecoverOrphans(ctx, pass, candidates, live, port, nil)
	}
	sweep("startup")
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(recheckAfter):
			sweep("recheck")
		}
	}()
}
