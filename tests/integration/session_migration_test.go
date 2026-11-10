//go:build integration

package integration

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// migrationsUpTo is the embedded migrations numbered at most n, so a database can
// be brought to the schema a release shipped before a later migration is applied
// to it.
func migrationsUpTo(t *testing.T, n int) fs.FS {
	t.Helper()
	subset := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, e := range entries {
		num, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil || !strings.HasSuffix(e.Name(), ".sql") || num > n {
			continue
		}
		b, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		subset[e.Name()] = &fstest.MapFile{Data: b}
	}
	return subset
}

// bareAdminPool starts a Postgres with nothing applied and returns a superuser
// pool, for a test that has to control which migrations have run.
func bareAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "pgvector/pgvector:pg17", // migrations/0022_retrieval.sql needs the vector extension
			ExposedPorts: []string{"5432/tcp"},
			Env:          map[string]string{"POSTGRES_USER": "nexus", "POSTGRES_PASSWORD": "nexus", "POSTGRES_DB": "nexus"},
			WaitingFor:   wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(120 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("postgres host: %v", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("postgres port: %v", err)
	}
	pool, err := pgxpool.New(ctx, fmt.Sprintf("postgres://nexus:nexus@%s:%s/nexus", host, port.Port()))
	if err != nil {
		t.Fatalf("connect as admin: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestMigrations_MovingFeatureColumnsOffSessionsKeepsTheirData runs 0026-0029
// against a database that already holds the old shape: a root session, a delegated
// child, a team member, a fork and a plan-pinned run, each carrying what the old
// columns said. It checks every fact arrives in its feature's table, that the sessions
// list's old filter (delegation_role = 'root') and its new one (no parent) pick the
// same rows, and that the aggregate is left without the columns and the references.
func TestMigrations_MovingFeatureColumnsOffSessionsKeepsTheirData(t *testing.T) {
	ctx := context.Background()
	pool := bareAdminPool(t)

	if _, err := store.Migrate(ctx, pool, migrationsUpTo(t, 25)); err != nil {
		t.Fatalf("migrate to the old shape: %v", err)
	}

	tenant, user := uuid.New(), uuid.New()
	root, delegated, member, fork, planned := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	team, plan := uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed the old shape: %v\n%s", err, sql)
		}
	}
	session := func(id uuid.UUID, extraCols, extraVals string, args ...any) {
		t.Helper()
		sql := `INSERT INTO sessions (session_id, session_key, tenant_id, surface_id, user_id, agent_id, agent_version, harness_digest, root_session_id` + extraCols + `)
			VALUES ($1, $5, $2, 'test', $3, $1, 1, '\x00', $4` + extraVals + `)`
		exec(sql, append([]any{id, tenant, user, id, id.String()}, args...)...)
	}
	exec(`INSERT INTO tenants (tenant_id, name) VALUES ($1, 'migration-test')`, tenant)
	session(root, "", "")
	exec(`INSERT INTO teams (team_id, tenant_id, name, coordinator_session_id, roster) VALUES ($1, $2, 'crew', $3, '[]')`, team, tenant, root)
	exec(`INSERT INTO orchestration_plans (plan_id, tenant_id, version, name, spec) VALUES ($1, $2, 3, 'p', '{}')`, plan, tenant)

	// The columns were stamped the way the old code stamped them: depth and role on a
	// spawned session, the team on a member, the fork fields on a fork, the pin on a
	// plan run; parent_session_id was never written.
	session(delegated, ", depth, delegation_role", ", 1, 'delegate'")
	exec(`UPDATE sessions SET root_session_id = $2 WHERE session_id = $1`, delegated, root)
	exec(`INSERT INTO delegations (delegation_id, tenant_id, parent_session_id, child_session_id, agent_id, task, scope_grant)
		VALUES ($1, $2, $3, $4, 'worker', 't', '[]')`, uuid.New(), tenant, root, delegated)
	session(member, ", depth, delegation_role, team_id", ", 1, 'team_member', $6", team)
	exec(`UPDATE sessions SET root_session_id = $2 WHERE session_id = $1`, member, root)
	session(fork, ", forked_from_session_id, fork_seq, fork_overrides", ", $6, 4, '{\"model_id\":\"m2\"}'", root)
	session(planned, ", plan_id, plan_version", ", $6, 3", plan)

	if _, err := store.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate the old shape forward: %v", err)
	}

	// Each fact is in its feature's table.
	var gotTeam uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT team_id FROM team_members WHERE session_id = $1`, member).Scan(&gotTeam); err != nil || gotTeam != team {
		t.Errorf("team_members[member] = %s, %v, want %s", gotTeam, err, team)
	}
	for _, c := range []struct {
		name string
		id   uuid.UUID
	}{{"delegated", delegated}, {"member", member}} {
		var r uuid.UUID
		var depth int
		if err := pool.QueryRow(ctx, `SELECT root_session_id, depth FROM session_lineage WHERE session_id = $1`, c.id).Scan(&r, &depth); err != nil || r != root || depth != 1 {
			t.Errorf("session_lineage[%s] = root %s depth %d, %v, want root %s depth 1", c.name, r, depth, err, root)
		}
	}
	var lineageRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_lineage`).Scan(&lineageRows); err != nil || lineageRows != 2 {
		t.Errorf("session_lineage has %d rows, %v, want 2: a session nobody spawned has none", lineageRows, err)
	}
	var from uuid.UUID
	var seq int64
	var model string
	if err := pool.QueryRow(ctx, `SELECT forked_from_session_id, fork_seq, overrides->>'model_id' FROM session_forks WHERE session_id = $1`, fork).Scan(&from, &seq, &model); err != nil || from != root || seq != 4 || model != "m2" {
		t.Errorf("session_forks[fork] = from %s at %d model %q, %v, want from %s at 4 model m2", from, seq, model, err, root)
	}
	var pinPlan uuid.UUID
	var pinVersion int
	if err := pool.QueryRow(ctx, `SELECT plan_id, plan_version FROM plan_sessions WHERE session_id = $1`, planned).Scan(&pinPlan, &pinVersion); err != nil || pinPlan != plan || pinVersion != 3 {
		t.Errorf("plan_sessions[planned] = %s version %d, %v, want %s version 3", pinPlan, pinVersion, err, plan)
	}

	// parent_session_id was never written; it is recovered from the delegation and the
	// team's coordinator, so the list hides exactly what delegation_role <> 'root' hid.
	rows, err := pool.Query(ctx, `SELECT session_id, parent_session_id FROM sessions ORDER BY session_id`)
	if err != nil {
		t.Fatalf("read parents: %v", err)
	}
	parents := map[uuid.UUID]*uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		var parent *uuid.UUID
		if err := rows.Scan(&id, &parent); err != nil {
			t.Fatalf("scan parent: %v", err)
		}
		parents[id] = parent
	}
	rows.Close()
	for id, want := range map[uuid.UUID]*uuid.UUID{root: nil, delegated: &root, member: &root, fork: nil, planned: nil} {
		got := parents[id]
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("parent_session_id of %s = %v, want %v", id, got, want)
		}
	}

	// The aggregate is left with neither the feature columns nor the references.
	assertAggregateIsFeatureFree(ctx, t, pool)
}

// aggregateColumns is every column the session aggregate may have: its identity,
// routing, the run configuration the kernel itself consumes (autonomy_level and
// conversational are kernel.RunConfig fields, read by every feature that resumes a
// run, so they belong to the session and not to the one feature that changes them),
// the status projection, and the one fact about the spawn tree it keeps. A column a
// feature stamps on a session goes in that feature's own table instead; adding one
// here is a decision, which is why this list is exact.
var aggregateColumns = []string{
	"session_id", "session_key", "tenant_id", "surface_id", "user_id", "audience_ref",
	"agent_id", "agent_version", "harness_digest",
	"data_label", "route_model_id", "route_reason", "execution_class", "priority", "region",
	"parent_session_id", "autonomy_level", "conversational",
	"taint_state", "status", "terminal_reason", "active_ms", "suspended_ms",
	"created_at", "updated_at",
}

// assertAggregateIsFeatureFree fails if sessions has a column outside
// aggregateColumns, or any table other than the aggregate itself (events, and
// sessions' own parent link) has a foreign key into it.
func assertAggregateIsFeatureFree(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	have := sessionColumnsInCatalogue(ctx, t, pool)
	want := map[string]bool{}
	for _, c := range aggregateColumns {
		want[c] = true
		if !have[c] {
			t.Errorf("sessions lacks column %s, which aggregateColumns lists", c)
		}
	}
	for c := range have {
		if !want[c] {
			t.Errorf("sessions has column %s, which a feature stamped on the aggregate: give it a table of its own, or add it to aggregateColumns with the reason it is the session's", c)
		}
	}
	if refs := tablesReferencingSessions(ctx, t, pool); len(refs) != 2 || refs[0] != "events" || refs[1] != "sessions" {
		t.Errorf("tables with a foreign key to sessions = %v, want only [events sessions]: a feature references a session by value", refs)
	}
}

func sessionColumnsInCatalogue(ctx context.Context, t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'sessions'`)
	if err != nil {
		t.Fatalf("read sessions columns: %v", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		cols[name] = true
	}
	return cols
}

// tablesReferencingSessions is every table with a foreign key whose target is
// sessions, sorted.
func tablesReferencingSessions(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT DISTINCT conrelid::regclass::text FROM pg_constraint WHERE contype = 'f' AND confrelid = 'sessions'::regclass`)
	if err != nil {
		t.Fatalf("read foreign keys: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
