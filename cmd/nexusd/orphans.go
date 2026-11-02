package main

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/queue"
)

const (
	// sessionLockTTL is the session-key serial lock's TTL. A worker renews it
	// every third of this while it runs (queue.WorkerConfig.RenewEvery), so it
	// is held for exactly as long as a worker is driving the session.
	sessionLockTTL = 30 * time.Second

	// orphanRecheckAfter is when the startup sweep runs once more. A worker that
	// died mid-run leaves its lock behind until the TTL passes, so the sweep at
	// startup has to skip that session as if it were live; this one, a TTL later,
	// finds the lock gone and recovers it.
	orphanRecheckAfter = sessionLockTTL + 5*time.Second
)

// orphanCandidate is a session the database still says is running.
type orphanCandidate struct {
	TenantID   uuid.UUID
	SessionID  uuid.UUID
	SessionKey string
}

// sessionLiveness says whether a worker is driving a session right now.
// *queueredis.SessionLock satisfies it.
type sessionLiveness interface {
	Held(ctx context.Context, sessionKey string) (bool, error)
}

// listRunningSessions is an admin, genuinely cross-tenant read, connected the
// same way cmd/nexusd's own listTenantIDs/runErase already establish precedent for.
func listRunningSessions(ctx context.Context, pool *pgxpool.Pool) ([]orphanCandidate, error) {
	rows, err := pool.Query(ctx, `SELECT session_id, tenant_id, session_key FROM sessions WHERE status = 'running'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []orphanCandidate
	for rows.Next() {
		var c orphanCandidate
		if err := rows.Scan(&c.SessionID, &c.TenantID, &c.SessionKey); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// recoverOrphans enqueues a queue.KindResume job for every candidate that no
// worker is driving, and returns how many. "Running in the database" alone does
// not make a session an orphan once there is more than one process: another
// process's worker may be mid-turn on it, and resuming that would only fail
// noisily at best. So a session whose lock is held is live and is skipped, and
// one whose liveness cannot be read is skipped too (unknown is not orphaned; the
// recheck covers it).
func recoverOrphans(ctx context.Context, pass string, candidates []orphanCandidate, live sessionLiveness, port queue.Port) (recovered int) {
	var skippedLive, unknown int
	for _, c := range candidates {
		held, err := live.Held(ctx, c.SessionKey)
		if err != nil {
			log.Warn().Err(err).Any("session_id", c.SessionID).Msg("nexusd: queue: cannot tell whether a running session is live; leaving it")
			unknown++
			continue
		}
		if held {
			skippedLive++
			continue
		}
		if _, err := port.Enqueue(ctx, queue.Job{TenantID: c.TenantID, SessionID: c.SessionID, SessionKey: c.SessionKey, Kind: queue.KindResume}); err != nil {
			log.Error().Err(err).Any("session_id", c.SessionID).Msg("nexusd: queue: enqueue resume for orphaned session failed")
			continue
		}
		recovered++
	}
	if recovered > 0 || skippedLive > 0 || unknown > 0 {
		log.Info().Str("pass", pass).Int("recovered", recovered).Int("skippedLiveelsewhere", skippedLive).Int("unknown", unknown).
			Msg("nexusd: queue: orphaned-session sweep")
	}
	return recovered
}

// startOrphanRecovery sweeps for orphaned sessions now, then once more after
// recheckAfter (see orphanRecheckAfter). It is deliberately not a periodic
// loop: an orphan whose resume fails for good (unresolved claims that need a
// human) would be re-enqueued forever. The first sweep runs before it returns,
// so recovery still happens before the workers start.
func startOrphanRecovery(ctx context.Context, list func(context.Context) ([]orphanCandidate, error), live sessionLiveness, port queue.Port, recheckAfter time.Duration) {
	sweep := func(pass string) {
		candidates, err := list(ctx)
		if err != nil {
			log.Error().Err(err).Str("pass", pass).Msg("nexusd: queue: list running sessions failed")
			return
		}
		recoverOrphans(ctx, pass, candidates, live, port)
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
