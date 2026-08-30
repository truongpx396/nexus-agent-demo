package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres implements Port against a connection pool that bypasses RLS — an
// admin/migration-role connection, exactly like cmd/nexusd's own
// listTenantIDs and runErase's session-to-tenant lookup already use
// (NEXUS_ADMIN_DATABASE_URL). Leasing "the next job for ANY tenant" is a
// genuinely cross-tenant admin read store.Store.InTenantTx has no way to
// express (its own doc comment: "the ONLY sanctioned way to scope a
// database operation to a TENANT"). Nothing about a job's own tenant
// boundary is weakened by this: the WORK a leased job drives (kernel.Run/
// Resume, via internal/runctl) still executes entirely inside
// store.Store.InTenantTx, scoped to that one job's tenant_id, exactly like
// every other write in this codebase.
type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) Enqueue(ctx context.Context, job Job) (Job, error) {
	if job.JobID == uuid.Nil {
		job.JobID = uuid.New()
	}
	if job.AvailableAt.IsZero() {
		job.AvailableAt = time.Now()
	}
	payload := job.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	err := p.pool.QueryRow(ctx, `
		INSERT INTO queue_jobs (job_id, tenant_id, session_id, session_key, kind, payload, available_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING status, attempts, created_at`,
		job.JobID, job.TenantID, job.SessionID, job.SessionKey, job.Kind, payload, job.AvailableAt,
	).Scan(&job.Status, &job.Attempts, &job.CreatedAt)
	if err != nil {
		return Job{}, fmt.Errorf("queue: enqueue job %s: %w", job.JobID, err)
	}
	return job, nil
}

// Lease runs the SKIP LOCKED claim inside its own short transaction: SELECT
// ... FOR UPDATE SKIP LOCKED finds one leasable row without blocking on (or
// being blocked by) any other worker's own concurrent Lease call, then the
// same transaction marks it leased before committing — so two workers
// racing this call can never both walk away with the same job.
func (p *Postgres) Lease(ctx context.Context, owner string, leaseFor time.Duration) (Job, bool, error) {
	var job Job
	found := false
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT job_id, tenant_id, session_id, session_key, kind, payload, status, attempts, available_at, created_at
			FROM queue_jobs
			WHERE status = 'pending' AND available_at <= now()
			ORDER BY available_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1`)
		err := row.Scan(&job.JobID, &job.TenantID, &job.SessionID, &job.SessionKey, &job.Kind,
			&job.Payload, &job.Status, &job.Attempts, &job.AvailableAt, &job.CreatedAt)
		if err != nil {
			if err == pgx.ErrNoRows {
				return nil // nothing leasable — found stays false
			}
			return fmt.Errorf("select leasable job: %w", err)
		}
		leaseExpires := time.Now().Add(leaseFor)
		if _, err := tx.Exec(ctx, `
			UPDATE queue_jobs
			SET status = 'leased', attempts = attempts + 1, lease_owner = $2, lease_expires_at = $3, updated_at = now()
			WHERE job_id = $1`,
			job.JobID, owner, leaseExpires,
		); err != nil {
			return fmt.Errorf("mark job %s leased: %w", job.JobID, err)
		}
		job.Status = StatusLeased
		job.Attempts++
		job.LeaseOwner = owner
		job.LeaseExpiresAt = &leaseExpires
		found = true
		return nil
	})
	if err != nil {
		return Job{}, false, fmt.Errorf("queue: lease: %w", err)
	}
	return job, found, nil
}

func (p *Postgres) Complete(ctx context.Context, jobID uuid.UUID) error {
	tag, err := p.pool.Exec(ctx, `UPDATE queue_jobs SET status = 'done', updated_at = now() WHERE job_id = $1`, jobID)
	if err != nil {
		return fmt.Errorf("queue: complete job %s: %w", jobID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("queue: complete job %s: no such job", jobID)
	}
	return nil
}

func (p *Postgres) Fail(ctx context.Context, jobID uuid.UUID, reason string, permanent bool, retryAt time.Time) error {
	status := StatusPending
	if permanent {
		status = StatusFailed
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE queue_jobs
		SET status = $2, available_at = $3, last_error = $4, lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
		WHERE job_id = $1`,
		jobID, status, retryAt, reason,
	)
	if err != nil {
		return fmt.Errorf("queue: fail job %s: %w", jobID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("queue: fail job %s: no such job", jobID)
	}
	return nil
}
