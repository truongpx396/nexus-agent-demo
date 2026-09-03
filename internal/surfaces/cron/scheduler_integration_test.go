//go:build integration

package cron

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// setupCronEnv mirrors internal/crypto/keystore_integration_test.go's own
// setupKeystoreEnv, but ALSO returns the raw migration-role pool: the
// scheduler's own TenantLister needs a genuine cross-tenant admin
// connection (nexus_app cannot make one — TenantLister's own doc comment),
// exactly like cmd/nexusd's real listTenantIDs.
func setupCronEnv(t *testing.T) (appPool, adminPool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	pgReq := testcontainers.ContainerRequest{
		Image:        "postgres:17",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "nexus",
			"POSTGRES_PASSWORD": "nexus",
			"POSTGRES_DB":       "nexus",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(120 * time.Second),
	}
	pgC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: pgReq, Started: true})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })

	host, err := pgC.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := pgC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}

	migrateDSN := fmt.Sprintf("postgres://nexus:nexus@%s:%s/nexus", host, port.Port())
	adminPool, err = pgxpool.New(ctx, migrateDSN)
	if err != nil {
		t.Fatalf("connect as migration role: %v", err)
	}
	t.Cleanup(adminPool.Close)
	if _, err := store.Migrate(ctx, adminPool, migrations.FS); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	appDSN := fmt.Sprintf("postgres://nexus_app:nexus_app@%s:%s/nexus", host, port.Port())
	appPool, err = pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect as nexus_app: %v", err)
	}
	t.Cleanup(appPool.Close)
	return appPool, adminPool
}

type poolTenantLister struct{ pool *pgxpool.Pool }

func (l poolTenantLister) ListTenantIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := l.pool.Query(ctx, `SELECT tenant_id FROM tenants`)
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

type fakeStarter struct {
	starts []RunRequest
}

func (f *fakeStarter) StartRun(_ context.Context, req RunRequest) (<-chan RunEvent, error) {
	f.starts = append(f.starts, req)
	ch := make(chan RunEvent)
	close(ch)
	return ch, nil
}

func insertTenant(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'cron-test')`, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
}

func insertDueSchedule(t *testing.T, s *store.Store, tenantID, userID uuid.UUID, name, cronExpr string, nextRunAt *time.Time) uuid.UUID {
	t.Helper()
	scheduleID := uuid.New()
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO cron_schedules (schedule_id, tenant_id, user_id, name, cron_expr, input, enabled, next_run_at) VALUES ($1, $2, $3, $4, $5, $6, true, $7)`,
			scheduleID, tenantID, userID, name, cronExpr, "do the daily thing", nextRunAt,
		)
		return err
	}); err != nil {
		t.Fatalf("insert schedule: %v", err)
	}
	return scheduleID
}

func TestScheduler_FiresDueScheduleAcrossMultipleTenants(t *testing.T) {
	appPool, adminPool := setupCronEnv(t)
	s := store.New(appPool)

	tenantA, tenantB := uuid.New(), uuid.New()
	insertTenant(t, adminPool, tenantA)
	insertTenant(t, adminPool, tenantB)

	userA, userB := uuid.New(), uuid.New()
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	dueA := insertDueSchedule(t, s, tenantA, userA, "due-a", "* * * * *", &past)
	insertDueSchedule(t, s, tenantB, userB, "not-due-b", "* * * * *", &future) // must NOT fire

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	starter := &fakeStarter{}
	sched := &Scheduler{
		Store:    s,
		KeyStore: crypto.NewKeyStore(kek),
		Starter:  starter,
		Tenants:  poolTenantLister{pool: adminPool},
	}

	sched.runOnce(context.Background())

	if len(starter.starts) != 1 {
		t.Fatalf("StartRun called %d times, want exactly 1 (only the due schedule)", len(starter.starts))
	}
	if starter.starts[0].TenantID != tenantA {
		t.Fatalf("fired for tenant %s, want %s", starter.starts[0].TenantID, tenantA)
	}
	if starter.starts[0].Input != "do the daily thing" {
		t.Fatalf("Input = %q, want the schedule's own input", starter.starts[0].Input)
	}

	// next_run_at must have advanced into the future — a second poll pass
	// must NOT fire the same schedule again immediately.
	var nextRunAt time.Time
	if err := s.InTenantTx(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT next_run_at FROM cron_schedules WHERE schedule_id = $1`, dueA).Scan(&nextRunAt)
	}); err != nil {
		t.Fatalf("load next_run_at: %v", err)
	}
	if !nextRunAt.After(time.Now()) {
		t.Fatalf("next_run_at = %s, want a time in the future", nextRunAt)
	}

	sched.runOnce(context.Background())
	if len(starter.starts) != 1 {
		t.Fatalf("StartRun called %d times after a second poll, want still 1 (next_run_at must have advanced)", len(starter.starts))
	}
}

func TestScheduler_UnparseableCronExprDisablesTheSchedule(t *testing.T) {
	appPool, adminPool := setupCronEnv(t)
	s := store.New(appPool)
	tenantID := uuid.New()
	insertTenant(t, adminPool, tenantID)
	userID := uuid.New()
	past := time.Now().Add(-time.Hour)
	scheduleID := insertDueSchedule(t, s, tenantID, userID, "broken", "not a cron expression", &past)

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	starter := &fakeStarter{}
	sched := &Scheduler{Store: s, KeyStore: crypto.NewKeyStore(kek), Starter: starter, Tenants: poolTenantLister{pool: adminPool}}

	sched.runOnce(context.Background())

	if len(starter.starts) != 0 {
		t.Fatalf("StartRun called %d times for an unparseable schedule, want 0", len(starter.starts))
	}
	var enabled bool
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT enabled FROM cron_schedules WHERE schedule_id = $1`, scheduleID).Scan(&enabled)
	}); err != nil {
		t.Fatalf("load enabled: %v", err)
	}
	if enabled {
		t.Fatal("an unparseable schedule was not disabled")
	}
}
