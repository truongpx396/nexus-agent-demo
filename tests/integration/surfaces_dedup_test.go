//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
	surfpg "github.com/truongpx396/nexus-agent-demo/internal/surfaces/postgres"
)

// TestInboundDedup pins provider-retry dedup through the real adapter: a
// redelivered message is acknowledged without a second dispatch, but only the
// exact (surface, delivery id) in the exact tenant counts as a repeat.
func TestInboundDedup(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	t.Cleanup(cleanup)
	st := store.New(pool)
	ctx := context.Background()
	dedup := surfpg.NewDedup(st)

	tenantA, tenantB := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{tenantA, tenantB} {
		if err := insertTenant(ctx, st, id); err != nil {
			t.Fatalf("insert tenant: %v", err)
		}
	}

	claim := func(tenantID uuid.UUID, surface, delivery string) bool {
		t.Helper()
		ok, err := dedup.Claim(ctx, tenantID, surface, delivery, "telegram:chat-42")
		if err != nil {
			t.Fatalf("Claim(%s, %s): %v", surface, delivery, err)
		}
		return ok
	}

	if !claim(tenantA, "telegram", "msg-1") {
		t.Error("the first delivery of a message was not claimed")
	}
	if claim(tenantA, "telegram", "msg-1") {
		t.Error("a redelivered message was claimed twice: it would dispatch a second paid turn")
	}
	if !claim(tenantA, "telegram", "msg-2") {
		t.Error("a different message from the same chat was treated as a repeat")
	}
	if !claim(tenantA, "zalo", "msg-1") {
		t.Error("the same id on another surface was treated as a repeat: ids are only unique within a provider")
	}
	if !claim(tenantB, "telegram", "msg-1") {
		t.Error("the same id in another tenant was treated as a repeat")
	}
	for i := 1; i <= 2; i++ {
		if !claim(tenantA, "telegram", "") {
			t.Errorf("message %d from a provider that sends no delivery id was treated as a repeat: every message would collapse into one", i)
		}
	}
}
