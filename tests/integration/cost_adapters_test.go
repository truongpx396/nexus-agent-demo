//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/cost/costtest"
	costpg "github.com/truongpx396/nexus-agent-demo/internal/cost/postgres"
	costredis "github.com/truongpx396/nexus-agent-demo/internal/cost/redis"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// TestPostgresLedger_SatisfiesTheLedgerContract runs the suite the in-memory
// Ledger passes (internal/cost/inmem) against the real adapter, through
// PgBouncer's transaction-pooling tier as nexus_app — the path production
// uses (docs/constitution.md, Principle VI) — so RLS, not a Go map, is what
// keeps one tenant out of another's rows.
func TestPostgresLedger_SatisfiesTheLedgerContract(t *testing.T) {
	pool, _ := setupCostEnv(t)
	st := store.New(pool)
	ctx := context.Background()

	inTenantTx := func(t *testing.T, tenantID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) {
		t.Helper()
		if err := st.InTenantTx(ctx, tenantID, fn); err != nil {
			t.Fatalf("tenant tx: %v", err)
		}
	}

	costtest.RunLedger(t, costtest.LedgerSubject{
		Ledger: costpg.New(st),
		NewTenant: func(t *testing.T) uuid.UUID {
			t.Helper()
			tenantID := uuid.New()
			if err := insertTenant(ctx, st, tenantID); err != nil {
				t.Fatalf("insert tenant: %v", err)
			}
			return tenantID
		},
		NewSession: func(t *testing.T, tenantID uuid.UUID) uuid.UUID {
			t.Helper()
			sessionID := uuid.New()
			inTenantTx(t, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return store.CreateSession(ctx, tx, store.Session{
					SessionID: sessionID, SessionKey: sessionID.String(), TenantID: tenantID,
					SurfaceID: "test", UserID: uuid.New(), AgentVersion: 1,
					HarnessDigest: []byte("test-digest"), DataLabel: "internal", RouteModelID: "fake",
					AutonomyLevel: "supervised",
				})
			})
			return sessionID
		},
		SeedPriceBook: func(t *testing.T, tenantID uuid.UUID, entries []cost.PriceBookEntry) {
			t.Helper()
			inTenantTx(t, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				for _, e := range entries {
					if err := costpg.InsertPriceBookEntry(ctx, tx, tenantID, e); err != nil {
						return err
					}
				}
				return nil
			})
		},
		SeedBudget: func(t *testing.T, tenantID uuid.UUID, scope cost.BudgetScope, ref *uuid.UUID, ceiling cost.Money) cost.Budget {
			t.Helper()
			var b cost.Budget
			inTenantTx(t, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				b, err = costpg.CreateBudget(ctx, tx, tenantID, scope, ref, ceiling)
				return err
			})
			return b
		},
	})
}

// TestRedisCounters_SatisfyTheCountersContract runs the suite the in-memory
// Counters pass against a real Redis, where the Lua scripts — not a mutex —
// provide the atomicity. This is what keeps the fake the Gate's unit tests
// rely on honest.
func TestRedisCounters_SatisfyTheCountersContract(t *testing.T) {
	_, redisClient := setupCostEnv(t)
	costtest.RunCounters(t, func(*testing.T) cost.Counters { return costredis.New(redisClient, 0) })
}
