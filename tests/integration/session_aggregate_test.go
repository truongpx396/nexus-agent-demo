//go:build integration

// The session aggregate stays free of what the features around it stamp on it
// (spec 005 T037): a team's roster, a session's place in the spawn tree, a fork's
// provenance and a plan's pin each live in the feature that owns them, and no
// feature table has a foreign key into sessions. These tests run the real
// migrations against a real Postgres and show the seams still do their jobs.
//
// Shares oversightRig/setupPostgresAndPgBouncer with oversight_test.go (same
// package).
package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/delegate"
	delegatepg "github.com/truongpx396/nexus-agent-demo/internal/delegate/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// TestSpawn_RecordsLineageAndKeepsSpawnedSessionsOutOfTheList is the whole path
// the spawn tree takes: a root session has no lineage record (it is its own root
// at depth 0, which is what lets the code that creates one stay ignorant of
// delegation), each spawn records one level deeper under the same root, the
// aggregate keeps only who spawned it, and the sessions list shows threads, not
// the sessions they spawned. The per-root count reads a root's own delegations
// and its descendants' as one.
func TestSpawn_RecordsLineageAndKeepsSpawnedSessionsOutOfTheList(t *testing.T) {
	r := setupOversightRig(t)
	prov := fake.New(contentScript("child done"), contentScript("grandchild done"))
	_, _, d, _ := buildDelegationRig(t, r, prov, nil)

	rootID, userID := uuid.New(), uuid.New()
	r.createSession(t, rootID, userID, "autonomous")

	spawn := func(parent uuid.UUID) uuid.UUID {
		t.Helper()
		id, err := d.Spawn(context.Background(), delegate.SpawnRequest{
			TenantID: r.tenantID, ParentSessionID: parent,
			AgentID: "worker", Task: "do work", ScopeGrant: []string{"platform/delegate@v1"},
		})
		if err != nil {
			t.Fatalf("Spawn under %s: %v", parent, err)
		}
		waitForStatus(t, r, id, eventlog.SessionStatusCompleted)
		return id
	}
	childID := spawn(rootID)
	grandchildID := spawn(childID)

	lineage := func(id uuid.UUID) (root uuid.UUID, depth int, rows int) {
		t.Helper()
		err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			if root, depth, err = delegatepg.LineageOf(ctx, tx, id); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM session_lineage WHERE session_id = $1`, id).Scan(&rows)
		})
		if err != nil {
			t.Fatalf("read lineage of %s: %v", id, err)
		}
		return root, depth, rows
	}
	for _, c := range []struct {
		name      string
		id        uuid.UUID
		wantDepth int
		wantRows  int
	}{
		{"root", rootID, 0, 0}, // nobody spawned it: no record, its own root
		{"child", childID, 1, 1},
		{"grandchild", grandchildID, 2, 1},
	} {
		root, depth, rows := lineage(c.id)
		if root != rootID || depth != c.wantDepth || rows != c.wantRows {
			t.Errorf("%s: lineage = root %s depth %d (%d record), want root %s depth %d (%d record)",
				c.name, root, depth, rows, rootID, c.wantDepth, c.wantRows)
		}
	}

	if got := r.getSession(t, childID).ParentSessionID; got == nil || *got != rootID {
		t.Errorf("child.ParentSessionID = %v, want %s", got, rootID)
	}
	if got := r.getSession(t, grandchildID).ParentSessionID; got == nil || *got != childID {
		t.Errorf("grandchild.ParentSessionID = %v, want %s", got, childID)
	}
	if got := r.getSession(t, rootID).ParentSessionID; got != nil {
		t.Errorf("root.ParentSessionID = %v, want nil: a thread's own session has no parent", got)
	}

	var listed []eventlog.Session
	if err := r.st.InTenantTx(context.Background(), r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		listed, err = store.ListSessionsForUser(ctx, tx, userID, 10)
		return err
	}); err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(listed) != 1 || listed[0].SessionID != rootID {
		t.Errorf("session list = %d sessions, want only the root %s: spawned sessions are reached through their parent, not listed as threads", len(listed), rootID)
	}

	// Neither parent ever ran, so there is no run for a finished child to resume
	// and both delegations stay pending; the point here is that the count finds the
	// root's own delegation (its parent has no lineage record) and the child's (its
	// parent does) under the one root.
	if open, total, err := d.CountForRoot(context.Background(), r.tenantID, rootID); err != nil || open != 2 || total != 2 {
		t.Errorf("CountForRoot(root) = open %d total %d, %v, want both delegations counted: the root's and its child's", open, total, err)
	}
}
