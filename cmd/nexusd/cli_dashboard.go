package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// runDashboard is `nexusd dashboard`: the golden-signal dashboard (README
// task 10.12) printed for every tenant — internal/obs.ComputeGoldenSignals
// does the actual querying; this is the CLI presentation of it. No
// DropTracker is threaded through here (this process's own Exporter, if
// any, is a separate instance with its own lifetime), so
// TelemetryAttrDropRate always prints 0 from this command specifically —
// documented in the printed output itself so that reads honestly as "not
// measured here," never as "measured zero."
func runDashboard(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	staleAfter := fs.Duration("stale-claim-after", 15*time.Minute, "how old an in_flight claim must be to count as unresolved")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dsn := envOr("NEXUS_DATABASE_URL", defaultAppDSN)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	st := store.New(pool)

	tenantIDs, err := listTenantIDs(ctx)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}

	for _, tenantID := range tenantIDs {
		signals, err := obs.ComputeGoldenSignals(ctx, st, tenantID, *staleAfter, nil, nil)
		if err != nil {
			return fmt.Errorf("compute golden signals for tenant %s: %w", tenantID, err)
		}
		printGoldenSignals(tenantID, signals)
	}
	return nil
}

func printGoldenSignals(tenantID uuid.UUID, s obs.GoldenSignals) {
	fmt.Printf("tenant %s:\n", tenantID)
	fmt.Printf("  terminal sessions: %d\n", s.TotalTerminalSessions)
	for reason, rate := range s.CompletionRateByReason {
		fmt.Printf("    %-20s %.1f%%\n", reason, rate*100)
	}
	fmt.Printf("  stuck rate:                %.1f%%\n", s.StuckRate*100)
	fmt.Printf("  cost-ceiling breach rate:  %.1f%%\n", s.CostCeilingBreachRate*100)
	fmt.Printf("  cache-read rate:           %.1f%%\n", s.CacheReadRate*100)
	fmt.Printf("  approval decision p50/p95: %dms / %dms\n", s.ApprovalP50DecisionMS, s.ApprovalP95DecisionMS)
	fmt.Printf("  approval_mismatch rate:    %.1f%%\n", s.ApprovalMismatchRate*100)
	fmt.Printf("  unresolved in-flight claims: %d\n", s.UnresolvedInFlightClaims)
	fmt.Printf("  telemetry attr-drop rate:  %.1f%% (not measured by this command — see internal/obs.DropTracker)\n", s.TelemetryAttrDropRate*100)
}
