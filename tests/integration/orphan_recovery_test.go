//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	runctlpg "github.com/truongpx396/nexus-agent-demo/internal/runctl/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// An orphan sweep that runs on a timer must not keep re-enqueueing a session whose
// resume can only refuse: a claim still in flight means a tool's effect may have
// left the process and only a person or a probe can say whether it landed. The
// session becomes a candidate again once the claim is resolved.
func TestListRunning_SkipsASessionWithAClaimInFlightUntilItIsResolved(t *testing.T) {
	ctx := context.Background()
	pool := bareAdminPool(t)
	if _, err := store.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	tenant := uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO tenants (tenant_id, name) VALUES ($1, 'orphans-test')`, tenant)

	newSession := func(status string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := store.CreateSession(ctx, tx, eventlog.Session{
			SessionID: id, SessionKey: id.String(), TenantID: tenant, SurfaceID: "test", UserID: uuid.New(),
			AgentVersion: 1, HarnessDigest: []byte("d"), DataLabel: "internal", RouteModelID: "fake", AutonomyLevel: "supervised",
		}); err != nil {
			t.Fatalf("create session: %v", err)
		}
		if err := store.UpdateSessionStatus(ctx, tx, id, status, nil); err != nil {
			t.Fatalf("set status: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return id
	}
	claim := func(session uuid.UUID, digest, status string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO claims (claim_id, tenant_id, session_id, tool_id, canonical_digest, status) VALUES ($1, $2, $3, 'platform/file_write@v1', $4, $5)`,
			id, tenant, session, []byte(digest), status)
		return id
	}

	plain := newSession(eventlog.SessionStatusRunning)
	inFlight := newSession(eventlog.SessionStatusRunning)
	claim(inFlight, "d1", "in_flight")
	settled := newSession(eventlog.SessionStatusRunning) // a claim that was completed is not a reason to wait
	claim(settled, "d2", "completed")
	mixed := newSession(eventlog.SessionStatusRunning) // one resolved claim does not hide an unresolved one
	claim(mixed, "d3", "completed")
	claim(mixed, "d4", "in_flight")
	newSession(eventlog.SessionStatusCompleted) // not running at all

	ids := func() map[uuid.UUID]bool {
		t.Helper()
		got, err := runctlpg.ListRunning(ctx, pool)
		if err != nil {
			t.Fatalf("ListRunning: %v", err)
		}
		found := map[uuid.UUID]bool{}
		for _, o := range got {
			found[o.SessionID] = true
			if o.TenantID != tenant || o.SessionKey != o.SessionID.String() {
				t.Errorf("orphan %v = %+v", o.SessionID, o)
			}
		}
		return found
	}

	got := ids()
	if len(got) != 2 || !got[plain] || !got[settled] {
		t.Fatalf("ListRunning = %v, want the plain session and the one whose only claim is completed", got)
	}

	// A person resolves the claim: the next sweep picks the session up.
	exec(`UPDATE claims SET status = 'abandoned', resolved_at = now() WHERE session_id = $1`, inFlight)
	if got := ids(); !got[inFlight] {
		t.Fatalf("ListRunning = %v, want the session back once its claim is resolved", got)
	}
}
