//go:build integration

// Phase 10 — golden-signal dashboard (README task 10.12). Proves
// internal/obs.ComputeGoldenSignals' SQL aggregation against real rows in
// every table it reads: sessions (completion/stuck rate), budget_decisions
// (cost-ceiling breach rate), cost_records (cache-read rate), approvals +
// events (approval latency + mismatch rate), claims (unresolved in-flight
// count). Shares setupOversightRig/insertTenant with phase5_oversight_test.go
// (same package).
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

func TestComputeGoldenSignals(t *testing.T) {
	r := setupOversightRig(t)
	ctx := context.Background()

	completed := uuid.New()
	stuck := uuid.New()
	userID := uuid.New()

	err := r.st.InTenantTx(ctx, r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, id := range []uuid.UUID{completed, stuck} {
			if err := store.CreateSession(ctx, tx, store.Session{
				SessionID: id, SessionKey: id.String(), TenantID: r.tenantID,
				SurfaceID: "test", UserID: userID, AgentID: uuid.Nil, AgentVersion: 1,
				HarnessDigest: []byte("test"), DataLabel: "internal", RouteModelID: "fake",
				AutonomyLevel: "autonomous",
			}); err != nil {
				return err
			}
		}
		completedReason := "completed"
		if err := store.UpdateSessionStatus(ctx, tx, completed, store.SessionStatusCompleted, &completedReason); err != nil {
			return err
		}
		stuckReason := "stuck_terminated"
		if err := store.UpdateSessionStatus(ctx, tx, stuck, store.SessionStatusFailed, &stuckReason); err != nil {
			return err
		}

		// budget_decisions: 2 allow, 1 refuse_ceiling -> breach rate 1/3.
		for _, decision := range []string{"allow", "allow", "refuse_ceiling"} {
			if _, err := tx.Exec(ctx, `
				INSERT INTO budget_decisions (budget_decision_id, tenant_id, session_id, decision, reason, reserved_micros, currency)
				VALUES ($1,$2,$3,$4,'test',0,'USD')`,
				uuid.New(), r.tenantID, completed, decision,
			); err != nil {
				return err
			}
		}

		// cost_records: 80 cache-read, 20 uncached -> cache-read rate 0.8.
		for _, rec := range []struct {
			meter string
			qty   int64
		}{{"input_cache_read", 80}, {"input_uncached", 20}} {
			if _, err := tx.Exec(ctx, `
				INSERT INTO cost_records (cost_record_id, tenant_id, session_id, meter, quantity, unit, minor_units, currency)
				VALUES ($1,$2,$3,$4,$5,'tokens',0,'USD')`,
				uuid.New(), r.tenantID, completed, rec.meter, rec.qty,
			); err != nil {
				return err
			}
		}

		// One decided approval: created 1000ms before it was decided.
		toolUseEventID := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO approvals (
				approval_id, tenant_id, session_id, tool_use_event_id, tool_id, ask_kind,
				canonical_digest, context_package, assignee, status, expires_at, created_at, decided_at, decided_by
			) VALUES ($1,$2,$3,$4,'platform/shell@v1','once','\x00','{}','tester','granted', now() + interval '1 hour',
				now() - interval '1000 milliseconds', now(), 'tester')`,
			uuid.New(), r.tenantID, completed, toolUseEventID,
		); err != nil {
			return err
		}

		// One approval_mismatch event against that same session -> mismatch rate 1/1.
		_, err := store.Append(ctx, tx, store.Event{
			EventID: uuid.New(), SessionID: completed, TenantID: r.tenantID,
			SchemaVersion: store.CurrentSchemaVersion, Type: store.EventApprovalMismatch,
			PayloadDigest: []byte{0}, KeyID: "test", Actor: store.ActorSystem,
		})
		if err != nil {
			return err
		}

		// One STALE in-flight claim (created well in the past) and one FRESH
		// one — only the stale one should count against a short staleness
		// window.
		if _, err := tx.Exec(ctx, `
			INSERT INTO claims (claim_id, tenant_id, session_id, tool_id, canonical_digest, status, created_at)
			VALUES ($1,$2,$3,'platform/shell@v1','\x01','in_flight', now() - interval '1 hour')`,
			uuid.New(), r.tenantID, completed,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO claims (claim_id, tenant_id, session_id, tool_id, canonical_digest, status, created_at)
			VALUES ($1,$2,$3,'platform/shell@v1','\x02','in_flight', now())`,
			uuid.New(), r.tenantID, completed,
		); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed fixtures: %v", err)
	}

	drops := obs.NewDropTracker()
	obs.FilterTracked(obs.Attrs{"tool.id": "x", "conversation.content": "secret"}, drops) // 1/2 dropped
	gap := 0.05

	got, err := obs.ComputeGoldenSignals(ctx, r.st, r.tenantID, 5*time.Minute, drops, &gap)
	if err != nil {
		t.Fatalf("ComputeGoldenSignals: %v", err)
	}

	if got.TotalTerminalSessions != 2 {
		t.Errorf("TotalTerminalSessions = %d, want 2", got.TotalTerminalSessions)
	}
	if got.CompletionRateByReason["completed"] != 0.5 || got.CompletionRateByReason["stuck_terminated"] != 0.5 {
		t.Errorf("CompletionRateByReason = %+v, want completed=0.5 stuck_terminated=0.5", got.CompletionRateByReason)
	}
	if got.StuckRate != 0.5 {
		t.Errorf("StuckRate = %v, want 0.5", got.StuckRate)
	}
	if want := 1.0 / 3.0; got.CostCeilingBreachRate < want-0.001 || got.CostCeilingBreachRate > want+0.001 {
		t.Errorf("CostCeilingBreachRate = %v, want ~%v", got.CostCeilingBreachRate, want)
	}
	if got.CacheReadRate != 0.8 {
		t.Errorf("CacheReadRate = %v, want 0.8", got.CacheReadRate)
	}
	if got.ApprovalP50DecisionMS < 900 || got.ApprovalP50DecisionMS > 1100 {
		t.Errorf("ApprovalP50DecisionMS = %d, want ~1000", got.ApprovalP50DecisionMS)
	}
	if got.ApprovalMismatchRate != 1.0 {
		t.Errorf("ApprovalMismatchRate = %v, want 1.0 (1 mismatch / 1 decided approval)", got.ApprovalMismatchRate)
	}
	if got.UnresolvedInFlightClaims != 1 {
		t.Errorf("UnresolvedInFlightClaims = %d, want 1 (only the stale one)", got.UnresolvedInFlightClaims)
	}
	if got.TelemetryAttrDropRate != 0.5 {
		t.Errorf("TelemetryAttrDropRate = %v, want 0.5", got.TelemetryAttrDropRate)
	}
	if got.HeldOutGap == nil || *got.HeldOutGap != 0.05 {
		t.Errorf("HeldOutGap = %v, want 0.05", got.HeldOutGap)
	}
}
