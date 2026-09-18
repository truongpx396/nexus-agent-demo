package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	"github.com/truongpx396/nexus-agent-demo/internal/queue"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/teams"
)

// startAnchorLoop runs the scheduled verifier task 5.3 asks for: every
// anchorInterval, anchor every tenant's new receipts and verify the whole
// chain, logging (not panicking on) a break or a gap — an alert a real
// deployment would ship to its paging system, not a reason to crash the
// process that is the source of truth for the very thing it's checking.
func startAnchorLoop(ctx context.Context, st *store.Store, chain *audit.Chain) (stop func()) {
	ticker := time.NewTicker(anchorInterval)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				anchorAndVerifyAllTenants(ctx, st, chain)
			}
		}
	}()
	return func() { close(done) }
}

func anchorAndVerifyAllTenants(ctx context.Context, st *store.Store, chain *audit.Chain) {
	tenantIDs, err := listTenantIDs(ctx)
	if err != nil {
		log.Error().Err(err).Msg("nexusd: list tenants for anchor/verify pass")
		return
	}
	for _, tenantID := range tenantIDs {
		if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, _, err := chain.Anchor(ctx, tx, tenantID); err != nil {
				return err
			}
			report, err := chain.Verify(ctx, tx, tenantID)
			if err != nil {
				return err
			}
			if !report.OK() {
				log.Error().Any("tenant_id", tenantID).Any("breaks", report.Breaks).Any("gaps", report.Gaps).Msg("nexusd: audit chain verification found a problem")
			}
			return nil
		}); err != nil {
			log.Error().Err(err).Any("tenant_id", tenantID).Msg("nexusd: anchor/verify pass failed")
		}
	}
}

// startTeamBackstopLoop runs README task 9.9's own wall-clock trigger: every
// teamBackstopSweepInterval, sweep every tenant for an 'active' team older
// than teamBackstopWindow and end it as 'aborted' — the same
// list-every-tenant-then-loop shape startAnchorLoop/anchorAndVerifyAllTenants
// already use for a periodic cross-tenant pass.
func startTeamBackstopLoop(ctx context.Context, teamsSvc *teams.Service) (stop func()) {
	ticker := time.NewTicker(teamBackstopSweepInterval)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				sweepTeamBackstopAllTenants(ctx, teamsSvc)
			}
		}
	}()
	return func() { close(done) }
}

func sweepTeamBackstopAllTenants(ctx context.Context, teamsSvc *teams.Service) {
	tenantIDs, err := listTenantIDs(ctx)
	if err != nil {
		log.Error().Err(err).Msg("nexusd: list tenants for team backstop sweep")
		return
	}
	for _, tenantID := range tenantIDs {
		if err := teamsSvc.SweepBackstop(ctx, tenantID, teamBackstopWindow); err != nil {
			log.Error().Err(err).Any("tenant_id", tenantID).Msg("nexusd: team backstop sweep failed")
		}
	}
}

// startIdleConversationSweepLoop runs the idle-conversation backstop: every
// idleConversationSweepInterval, sweep every tenant for a conversational
// session sitting in store.SessionStatusAwaitingInput past
// idleConversationWindow and end it via runctl.Control.EndIdleConversation
// — the same list-every-tenant-then-loop shape startAnchorLoop/
// startTeamBackstopLoop already use for a periodic cross-tenant pass.
func startIdleConversationSweepLoop(ctx context.Context, ctl *runctl.Control) (stop func()) {
	ticker := time.NewTicker(idleConversationSweepInterval)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				sweepIdleConversationsAllTenants(ctx, ctl)
			}
		}
	}()
	return func() { close(done) }
}

func sweepIdleConversationsAllTenants(ctx context.Context, ctl *runctl.Control) {
	tenantIDs, err := listTenantIDs(ctx)
	if err != nil {
		log.Error().Err(err).Msg("nexusd: list tenants for idle-conversation sweep")
		return
	}
	olderThan := time.Now().Add(-idleConversationWindow)
	for _, tenantID := range tenantIDs {
		var staleIDs []uuid.UUID
		if err := ctl.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			staleIDs, err = store.ListStaleAwaitingInputSessionIDs(ctx, tx, olderThan)
			return err
		}); err != nil {
			log.Error().Err(err).Any("tenant_id", tenantID).Msg("nexusd: idle-conversation sweep: list stale sessions failed")
			continue
		}
		for _, sessionID := range staleIDs {
			if err := ctl.EndIdleConversation(ctx, tenantID, sessionID); err != nil {
				log.Error().Err(err).Any("tenant_id", tenantID).Any("session_id", sessionID).Msg("nexusd: idle-conversation sweep: end session failed")
			}
		}
	}
}

// listTenantIDs enumerates every tenant in the system — genuinely
// cross-tenant, unlike everything else in this binary (store.Store.
// InTenantTx is "the only sanctioned way to scope a database operation to
// a tenant," per its own doc comment, and has no notion of "every
// tenant"). st's own pool connects as nexus_app through PgBouncer, an
// ordinary role RLS-restricted to whatever app.tenant_id the CURRENT
// transaction set — which is nothing, here, on purpose: there is no tenant
// to scope to yet, that's the whole point of this query. So this one admin
// operation connects directly as the migration superuser instead, exactly
// like runMigrate already does for admin-only DDL — the anchor/verify pass
// (README task 5.3) is the same kind of platform-operator action, not a
// tenant-scoped one.
func listTenantIDs(ctx context.Context) ([]uuid.UUID, error) {
	dsn := envOr("NEXUS_ADMIN_DATABASE_URL", envOr("NEXUS_MIGRATE_DATABASE_URL", defaultMigrateDSN))
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect as admin: %w", err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, `SELECT tenant_id FROM tenants`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// startQueueWorkers wires internal/queue's worker pool (README tasks
// 6.1-6.2) to internal/runctl.Control.Resume: the durable, crash-recoverable
// path a session's turn loop continues through after this process (or a
// prior one) died mid-turn. It also sweeps for sessions this process's own
// PREVIOUS life left stuck in "running" (session status is written
// synchronously at every turn boundary; a row still reading "running" at
// startup can only mean the process that was driving it never got to write
// anything past that point) and enqueues a resume job for each — the
// concrete trigger behind README §6's demo line: "kill -9 the worker
// mid-tool-call -> the job re-queues and resumes from the checkpoint."
//
// Deliberately NOT wired here: a fresh interactive run
// (POST /v1/runs, kernelRunStarter.StartRun) stays on its own existing
// synchronous fast path, never enqueued — a queued job's payload carries no
// sealed envelope the way events.payload does (migrations/0011_queue.sql's
// own doc comment, still true of both queue.Port adapters below), so it
// must never carry a plaintext opening message. Recovering an orphaned
// FRESH run (one that never got far enough to suspend or checkpoint) is
// exactly what the sweep below already covers: its status is "running"
// either way.
func startQueueWorkers(ctx context.Context, st *store.Store, redisClient *redis.Client, lock *queue.SessionLock, ctl *runctl.Control, delegations *delegate.Delegations, teamsSvc *teams.Service) (stop func()) {
	adminDSN := envOr("NEXUS_ADMIN_DATABASE_URL", envOr("NEXUS_MIGRATE_DATABASE_URL", defaultMigrateDSN))
	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		log.Error().Err(err).Msg("nexusd: queue: connect as admin failed; the worker pool is NOT running (fresh runs still work; crash recovery does not)")
		return func() {}
	}

	port, err := newQueuePort(ctx, adminPool, redisClient)
	if err != nil {
		log.Error().Err(err).Msg("nexusd: queue: configure backend failed; the worker pool is NOT running (fresh runs still work; crash recovery does not)")
		adminPool.Close()
		return func() {}
	}
	runner := &queueRunner{ctl: ctl, delegations: delegations, teams: teamsSvc}

	// recoverOrphanedSessions still queries Postgres directly (sessions
	// live there regardless of which queue backend carries the resume
	// job) and enqueues through the Port interface — identical either way.
	recoverOrphanedSessions(ctx, adminPool, port)

	numWorkers := 2
	workerCtx, cancel := context.WithCancel(ctx)
	for i := 0; i < numWorkers; i++ {
		w := queue.NewWorker(queue.WorkerConfig{
			Port: port, Lock: lock, Runner: runner,
			Owner: fmt.Sprintf("nexusd-worker-%d-%d", os.Getpid(), i),
		})
		go w.Run(workerCtx)
	}
	return func() {
		cancel()
		adminPool.Close()
	}
}

// newQueuePort selects the queue backend (docs/build-phases.md Phase 18).
// NEXUS_QUEUE_BACKEND=redis (the default, unset reproduces it) wires
// internal/queue.RedisStream — consumer-group leasing, continuous
// XAUTOCLAIM-based reclaim of an abandoned job from ANY dead consumer
// rather than only the sweep below at THIS process's own next startup, and
// no poll-driven admin-Postgres traffic for the lease/complete/fail path
// itself. NEXUS_QUEUE_BACKEND=postgres keeps the original SKIP LOCKED
// adapter (postgres.go) wired instead — never removed, still exercised
// directly by tests/integration/phase6_reliability_test.go, and a
// one-env-var rollback if the Redis backend ever needs to be ruled out
// against a live deployment.
func newQueuePort(ctx context.Context, adminPool *pgxpool.Pool, redisClient *redis.Client) (queue.Port, error) {
	switch backend := envOr("NEXUS_QUEUE_BACKEND", "redis"); backend {
	case "redis":
		p := queue.NewRedisStream(redisClient)
		if err := p.EnsureGroup(ctx); err != nil {
			return nil, fmt.Errorf("ensure redis stream consumer group: %w", err)
		}
		return p, nil
	case "postgres":
		return queue.NewPostgres(adminPool), nil
	default:
		return nil, fmt.Errorf("unknown NEXUS_QUEUE_BACKEND %q (want \"redis\" or \"postgres\")", backend)
	}
}

// recoverOrphanedSessions enqueues a KindResume job for every session this
// (or a prior) process left in "running" status — an admin, genuinely
// cross-tenant read, connected the same way cmd/nexusd's own
// listTenantIDs/runErase already establish precedent for.
func recoverOrphanedSessions(ctx context.Context, adminPool *pgxpool.Pool, port queue.Port) {
	rows, err := adminPool.Query(ctx, `SELECT session_id, tenant_id, session_key FROM sessions WHERE status = 'running'`)
	if err != nil {
		log.Error().Err(err).Msg("nexusd: queue: list orphaned running sessions failed")
		return
	}
	defer rows.Close()
	var recovered int
	for rows.Next() {
		var sessionID, tenantID uuid.UUID
		var sessionKey string
		if err := rows.Scan(&sessionID, &tenantID, &sessionKey); err != nil {
			log.Error().Err(err).Msg("nexusd: queue: scan orphaned session failed")
			continue
		}
		if _, err := port.Enqueue(ctx, queue.Job{TenantID: tenantID, SessionID: sessionID, SessionKey: sessionKey, Kind: queue.KindResume}); err != nil {
			log.Error().Err(err).Any("session_id", sessionID).Msg("nexusd: queue: enqueue resume for orphaned session failed")
			continue
		}
		recovered++
	}
	if recovered > 0 {
		log.Info().Int("count", recovered).Msg("nexusd: queue: enqueued resume jobs for orphaned running sessions")
	}
}

// queueRunner implements queue.Runner over internal/runctl.Control.Resume —
// the only Kind this demo's queue ever carries; see startQueueWorkers' own
// doc comment for why fork/steer are driven synchronously via REST instead
// of through the queue.
type queueRunner struct {
	ctl         *runctl.Control
	delegations *delegate.Delegations
	teams       *teams.Service
}

func (r *queueRunner) Run(ctx context.Context, job queue.Job) error {
	var lastErr error
	for ev, err := range r.ctl.Resume(ctx, job.TenantID, job.SessionID) {
		if err != nil {
			lastErr = err
			break
		}
		_ = ev
	}
	if lastErr != nil {
		return lastErr
	}

	// A crash-recovered session that is ALSO a delegation's child (README
	// task 8.10) may have just reached its own terminal state via the
	// resume above — OnChildTerminal is a documented no-op for every other
	// session (the overwhelming majority), and only does real work here.
	if r.delegations != nil {
		if err := r.delegations.OnChildTerminal(ctx, job.TenantID, job.SessionID); err != nil {
			log.Error().Err(err).Any("session_id", job.SessionID).Msg("nexusd: queue: resolve delegation after crash-recovered child failed")
		}
	}
	// A crash-recovered session that is ALSO a team member (README task 9.9)
	// may have just reached its own terminal state via the resume above —
	// OnMemberTerminal is a documented no-op for every other session, and
	// only does real work here, mirroring OnChildTerminal's own call above.
	if r.teams != nil {
		if err := r.teams.OnMemberTerminal(ctx, job.TenantID, job.SessionID); err != nil {
			log.Error().Err(err).Any("session_id", job.SessionID).Msg("nexusd: queue: resolve team after crash-recovered member failed")
		}
	}

	// A checkpoint after every leased job returns (README task 6.3) — a
	// durable, denormalized pointer a FUTURE resume can consult fast,
	// never the source of truth for any one field (its own doc comment,
	// internal/store/checkpoint.go).
	var harnessDigest []byte
	err := r.ctl.Store.InTenantTx(ctx, job.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		sess, err := store.GetSession(ctx, tx, job.SessionID)
		if err != nil {
			return err
		}
		harnessDigest = sess.HarnessDigest
		claims, err := store.ListInFlightClaims(ctx, tx, job.SessionID)
		if err != nil {
			return err
		}
		var openClaim *uuid.UUID
		if len(claims) > 0 {
			openClaim = &claims[0].ClaimID
		}
		history, err := store.ListEvents(ctx, tx, job.SessionID)
		if err != nil {
			return err
		}
		var coveredSeq int64
		if len(history) > 0 {
			coveredSeq = history[len(history)-1].Seq
		}
		_, err = store.SaveCheckpoint(ctx, tx, store.Checkpoint{
			TenantID: job.TenantID, SessionID: job.SessionID, CoveredSeq: coveredSeq,
			OpenClaimID: openClaim, HarnessDigest: harnessDigest,
		})
		return err
	})
	if err != nil {
		log.Error().Err(err).Any("job_id", job.JobID).Msg("nexusd: queue: save checkpoint after run failed")
	}
	return nil
}
