//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	costpg "github.com/truongpx396/nexus-agent-demo/internal/cost/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	obspg "github.com/truongpx396/nexus-agent-demo/internal/obs/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// TestReports_ToolCallCountsAndModelSpend pins the two /metrics breakdowns that
// had no test of their own (the golden signals have phase10_dashboard_test.go):
// tool_use events group by tool_id, cost_records group by model and currency
// with the four token classes split out, and neither leaks another tenant's rows.
func TestReports_ToolCallCountsAndModelSpend(t *testing.T) {
	r := setupOversightRig(t)
	ctx := context.Background()
	reports := obspg.New(r.st)

	otherTenant := uuid.New()
	if err := insertTenant(ctx, r.st, otherTenant); err != nil {
		t.Fatalf("insert other tenant: %v", err)
	}

	seed := func(tenantID uuid.UUID, tools map[string]int, usage []cost.CostRecord) {
		t.Helper()
		sessionID := uuid.New()
		err := r.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if err := store.CreateSession(ctx, tx, store.Session{
				SessionID: sessionID, SessionKey: sessionID.String(), TenantID: tenantID,
				SurfaceID: "test", UserID: uuid.New(), AgentVersion: 1,
				HarnessDigest: []byte("test"), DataLabel: "internal", RouteModelID: "fake",
				AutonomyLevel: "autonomous",
			}); err != nil {
				return err
			}
			for tool, n := range tools {
				for i := 0; i < n; i++ {
					tool := tool
					if _, err := store.Append(ctx, tx, eventlog.Event{
						EventID: uuid.New(), SessionID: sessionID, TenantID: tenantID,
						SchemaVersion: eventlog.CurrentSchemaVersion, Type: eventlog.EventToolUse, ToolID: &tool,
						PayloadDigest: []byte{0}, KeyID: "test", Actor: eventlog.ActorSystem,
					}); err != nil {
						return err
					}
				}
			}
			return costpg.RecordUsage(ctx, tx, tenantID, sessionID, usage)
		})
		if err != nil {
			t.Fatalf("seed tenant %s: %v", tenantID, err)
		}
	}
	usd := func(micros int64) cost.Money { return cost.Money{Micros: micros, Currency: cost.DefaultCurrency} }
	rec := func(meter cost.MeterID, qty int64, model string, micros int64) cost.CostRecord {
		return cost.CostRecord{Meter: meter, Quantity: qty, Unit: "tokens", ModelID: model, Cost: usd(micros)}
	}

	seed(r.tenantID, map[string]int{"platform/shell@v1": 2, "platform/web@v1": 1}, []cost.CostRecord{
		rec(cost.MeterInputUncached, 100, "model-a", 300),
		rec(cost.MeterInputCacheRead, 400, "model-a", 40),
		rec(cost.MeterOutput, 50, "model-a", 750),
		rec(cost.MeterInputUncached, 10, "model-b", 30),
		{Meter: cost.MeterSandboxSeconds, Quantity: 5, Unit: "seconds", Cost: usd(999)}, // no model: not a model spend
	})
	seed(otherTenant, map[string]int{"platform/shell@v1": 7}, []cost.CostRecord{rec(cost.MeterOutput, 1, "model-a", 1_000_000)})

	counts, err := reports.ToolCallCounts(ctx, r.tenantID)
	if err != nil {
		t.Fatalf("ToolCallCounts: %v", err)
	}
	if len(counts) != 2 || counts["platform/shell@v1"] != 2 || counts["platform/web@v1"] != 1 {
		t.Errorf("tool call counts = %v, want shell=2 web=1 and nothing from the other tenant's 7", counts)
	}

	spend, err := reports.ModelSpend(ctx, r.tenantID)
	if err != nil {
		t.Fatalf("ModelSpend: %v", err)
	}
	byModel := map[string]int{}
	for i, m := range spend {
		byModel[m.ModelID] = i
	}
	if len(spend) != 2 {
		t.Fatalf("model spend = %+v, want exactly model-a and model-b (a NULL-model meter is excluded)", spend)
	}
	a := spend[byModel["model-a"]]
	if a.InputUncached != 100 || a.InputCacheRead != 400 || a.Output != 50 || a.InputCacheWrite != 0 || a.CostMinorUnits != 1_090 || a.Currency != cost.DefaultCurrency {
		t.Errorf("model-a = %+v, want uncached 100, cache-read 400, output 50, cost 1090 USD", a)
	}
	if b := spend[byModel["model-b"]]; b.InputUncached != 10 || b.CostMinorUnits != 30 {
		t.Errorf("model-b = %+v, want uncached 10, cost 30", b)
	}
}
