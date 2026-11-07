package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	auditpg "github.com/truongpx396/nexus-agent-demo/internal/audit/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/queue"
	queueredis "github.com/truongpx396/nexus-agent-demo/internal/queue/redis"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	runctlpg "github.com/truongpx396/nexus-agent-demo/internal/runctl/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/teams"
	"github.com/truongpx396/nexus-agent-demo/internal/tenancy"
	tenancypg "github.com/truongpx396/nexus-agent-demo/internal/tenancy/postgres"
)

const (
	// sessionLockTTL is the session-key serial lock's TTL. A worker renews it
	// every third of this while it runs (queue.WorkerConfig.RenewEvery), so it
	// is held for exactly as long as a worker is driving the session.
	sessionLockTTL = 30 * time.Second

	// orphanRecheckAfter is when the startup orphan sweep runs once more. A
	// worker that died mid-run leaves its lock behind until the TTL passes, so
	// the sweep at startup has to skip that session as if it were live; this
	// one, a TTL later, finds the lock gone and recovers it.
	orphanRecheckAfter = sessionLockTTL + 5*time.Second
)

// adminDSN is the administrative connection string, for the few operations
// that are genuinely cross-tenant and so cannot run as the RLS-restricted
// runtime role.
func adminDSN() string {
	return envOr("NEXUS_ADMIN_DATABASE_URL", envOr("NEXUS_MIGRATE_DATABASE_URL", defaultMigrateDSN))
}

// tenantDirectory is the platform operator's view of the tenants: it lists them
// (tenancy.Directory) and says which one owns a session (tenancy.SessionLocator).
func tenantDirectory() tenancypg.Directory { return tenancypg.Directory{DSN: adminDSN()} }

// startPeriodic runs fn every interval until the returned stop is called. It is
// the one ticker loop the platform's scheduled passes share; what a pass does,
// and for which tenants, is the owning package's business (tenancy.ForEach
// visits them), so this file only decides when.
func startPeriodic(ctx context.Context, interval time.Duration, fn func(context.Context)) (stop func()) {
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fn(ctx)
			}
		}
	}()
	return func() { close(done) }
}

// startAnchorLoop runs the scheduled verifier task 5.3 asks for: every
// anchorInterval, anchor every tenant's new receipts and verify the whole
// chain. A break or a gap is logged, not fatal — an alert a real deployment
// would ship to its paging system, not a reason to crash the process that is
// the source of truth for the very thing it is checking.
func startAnchorLoop(ctx context.Context, dir tenancy.Directory, audit auditpg.Scoped) (stop func()) {
	return startPeriodic(ctx, anchorInterval, func(ctx context.Context) {
		tenancy.ForEach(ctx, dir, "audit anchor/verify", audit.AnchorAndVerify)
	})
}

// startTeamBackstopLoop runs README task 9.9's own wall-clock trigger: every
// teamBackstopSweepInterval, sweep every tenant for an 'active' team older than
// teamBackstopWindow and end it as 'aborted'.
func startTeamBackstopLoop(ctx context.Context, dir tenancy.Directory, teamsSvc *teams.Service) (stop func()) {
	return startPeriodic(ctx, teamBackstopSweepInterval, func(ctx context.Context) {
		tenancy.ForEach(ctx, dir, "team backstop", func(ctx context.Context, tenantID uuid.UUID) error {
			return teamsSvc.SweepBackstop(ctx, tenantID, teamBackstopWindow)
		})
	})
}

// startIdleConversationSweepLoop runs the idle-conversation backstop: every
// idleConversationSweepInterval, sweep every tenant for a conversational session
// sitting in awaiting_input past idleConversationWindow and end it
// (runctl.Control.SweepIdle).
func startIdleConversationSweepLoop(ctx context.Context, dir tenancy.Directory, ctl *runctl.Control) (stop func()) {
	return startPeriodic(ctx, idleConversationSweepInterval, func(ctx context.Context) {
		olderThan := time.Now().Add(-idleConversationWindow)
		tenancy.ForEach(ctx, dir, "idle conversation sweep", func(ctx context.Context, tenantID uuid.UUID) error {
			return ctl.SweepIdle(ctx, tenantID, olderThan)
		})
	})
}

// startQueueWorkers wires internal/queue's worker pool (README tasks
// 6.1-6.2) to internal/runctl.Control.Resume: the durable, crash-recoverable
// path a session's turn loop continues through, whether it's continuing
// after this process (or a prior one) died mid-turn (queue.KindResume), or
// picking up a fresh run/conversational follow-up some HTTP-handling
// process already seeded/appended and enqueued (queue.KindStart/
// KindConverse — kernelRunStarter.StartRun, internal/runctl.Control.
// ResumeConversation). By the time a worker leases any of these three, a
// fresh-started, crash-recovered, and human-followed-up session all look
// identical — their own seed/message event is already durably in the
// Postgres event log — so every Kind dispatches through the same
// ctl.Resume call; the switch below exists purely so logs/metrics can still
// tell them apart.
//
// It also sweeps for sessions a worker that died left stuck in "running"
// (runctl.StartRecovery) and enqueues a resume job for each — the
// concrete trigger behind README §6's demo line: "kill -9 the worker
// mid-tool-call -> the job re-queues and resumes from the checkpoint." A row
// still reading "running" is only an orphan if no worker is driving it, which
// with more than one process the row alone no longer says: so a session whose
// session lock is held (a live worker renews it every turn) is skipped, and a
// second sweep runs after the lock's TTL for the sessions a just-dead worker's
// lock was still covering. This sweep needs no changes for
// KindStart/KindConverse: kernel.Kernel.Seed's own updateStatus call (the
// same one Run always had) is what flips a session from "queued" to
// "running" in the first place, so a session only ever reaches "running"
// once it durably has at least its seed/message event — recovering an
// orphaned FRESH run is already exactly what this sweep covers.
func startQueueWorkers(ctx context.Context, redisClient *redis.Client, port queue.Port, srv *rest.Server, ctl *runctl.Control) (stop func()) {
	adminPool, err := pgxpool.New(ctx, adminDSN())
	if err != nil {
		log.Error().Err(err).Msg("nexusd: queue: connect as admin failed; the worker pool is NOT running (fresh runs will be seeded but never actually driven, and crash recovery does not happen)")
		return func() {}
	}

	lock := queueredis.NewSessionLock(redisClient, sessionLockTTL)
	runner := &queueRunner{ctl: ctl, srv: srv}

	workerCtx, cancel := context.WithCancel(ctx)
	runctl.StartRecovery(workerCtx, func(ctx context.Context) ([]runctl.Orphan, error) { return runctlpg.ListRunning(ctx, adminPool) }, lock, port, orphanRecheckAfter)

	numWorkers := 2
	for i := 0; i < numWorkers; i++ {
		w := queue.NewWorker(queue.WorkerConfig{
			Port: port, Lock: lock, Runner: runner, RenewEvery: sessionLockTTL / 3,
			Owner: fmt.Sprintf("nexusd-worker-%d-%d", os.Getpid(), i),
		})
		go w.Run(workerCtx)
	}
	return func() {
		cancel()
		adminPool.Close()
	}
}

// queueRunner implements queue.Runner over internal/runctl.Control.Resume —
// every Kind this queue carries dispatches through it identically (see
// startQueueWorkers' own doc comment); Fork/Cancel/TightenAutonomy/Steer
// are driven synchronously via REST instead of through the queue, since
// none of them drive the turn loop themselves.
type queueRunner struct {
	ctl *runctl.Control

	// srv publishes every event this job's own turn loop produces — the
	// same PublishEvent call internal/surfaces/rest's own synchronous fast
	// path (publishUntilDone) makes, so an SSE client sees a queued run's
	// real output (and its eventual EventTerminal) regardless of which
	// process's worker actually drove it, and approval-outbox delivery/
	// terminal-span emission fire exactly once either way.
	srv *rest.Server
}

func (r *queueRunner) Run(ctx context.Context, job queue.Job) error {
	for ev, err := range r.ctl.Resume(ctx, job.TenantID, job.SessionID) {
		if r.srv != nil {
			r.srv.PublishEvent(job.TenantID, job.SessionID, ev, err)
		}
		if err != nil {
			return err
		}
	}

	// A crash-recovered session may ALSO be a delegation's child (README task
	// 8.10) or a team member (task 9.9) that just reached its own terminal
	// state via the resume above; whoever cares has registered as a
	// runctl.SettledObserver, and each is a no-op for every other session.
	r.ctl.NotifySettled(ctx, job.TenantID, job.SessionID)

	// A checkpoint after every leased job returns (README task 6.3) — a
	// durable pointer a FUTURE resume can consult fast, never the source of
	// truth for any one field.
	if err := r.ctl.SaveCheckpoint(ctx, job.TenantID, job.SessionID); err != nil {
		log.Error().Err(err).Any("job_id", job.JobID).Msg("nexusd: queue: save checkpoint after run failed")
	}
	return nil
}
