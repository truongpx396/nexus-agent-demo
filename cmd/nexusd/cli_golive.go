package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/nexus-agent-demo/evals"
	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// runGoLive is `nexusd go-live`: docs/go-live.md's checklist, scripted
// (README task 10.13). Every AUTOMATED item there is checked here, per
// tenant; every MANUAL item is printed as a named reminder, never silently
// skipped — a checklist that quietly omits what it can't verify is worse
// than no checklist, because it still prints green.
func runGoLive(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("go-live", flag.ExitOnError)
	staleAfter := fs.Duration("stale-claim-after", 15*time.Minute, "how old an in_flight claim must be to count as unresolved")
	cacheReadFloor := fs.Float64("cache-read-floor", 0.90, "minimum acceptable cache-read rate (docs/go-live.md item 8)")
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

	ok := true
	check := func(passed bool, format string, args ...any) {
		status := "PASS"
		if !passed {
			status, ok = "FAIL", false
		}
		fmt.Printf("[%s] %s\n", status, fmt.Sprintf(format, args...))
	}

	// Item 1: attributable audit log.
	signer := audit.NewSignerClient(envOr("NEXUS_SIGNERD_SOCKET", defaultSignerdSocket))
	chain := audit.NewChain(signer)
	tenantIDs, err := listTenantIDs(ctx)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	for _, tenantID := range tenantIDs {
		var report audit.Report
		err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, _, err := chain.Anchor(ctx, tx, tenantID); err != nil {
				return err
			}
			var verr error
			report, verr = chain.Verify(ctx, tx, tenantID)
			return verr
		})
		check(err == nil && report.OK(), "item 1 (attributable audit log): tenant %s chain verifies", tenantID)
	}

	// Item 2: vaulted per-tenant secrets — KEK loads, signerd answers.
	kekPath := envOr("NEXUS_KEK_PATH", defaultKEKPath)
	_, kekErr := func() (crypto.KEK, error) {
		f, ferr := os.Open(kekPath) //nolint:gosec // operator-controlled config path
		if ferr != nil {
			return crypto.KEK{}, ferr
		}
		defer f.Close() //nolint:errcheck
		return crypto.LoadKEK(f)
	}()
	check(kekErr == nil, "item 2a (vaulted secrets): KEK loads from %s", kekPath)
	_, _, signerErr := signer.PublicKey(ctx)
	check(signerErr == nil, "item 2b (vaulted secrets): signerd reachable and answering")

	// Item 5: per-task/per-tenant cost ceilings.
	for _, tenantID := range tenantIDs {
		var tenantBudgets int
		err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM budgets WHERE scope = 'tenant'`).Scan(&tenantBudgets)
		})
		check(err == nil && tenantBudgets > 0, "item 5 (cost ceilings): tenant %s has a tenant-scoped budget configured", tenantID)
	}

	// Items 6 and 8, from the golden-signal dashboard.
	for _, tenantID := range tenantIDs {
		signals, serr := obs.ComputeGoldenSignals(ctx, st, tenantID, *staleAfter, nil, nil)
		if serr != nil {
			check(false, "item 6/8 (dashboard): tenant %s: %v", tenantID, serr)
			continue
		}
		check(signals.UnresolvedInFlightClaims == 0, "item 6 (failure classification + resume + stuck detection): tenant %s has %d unresolved in-flight claim(s)", tenantID, signals.UnresolvedInFlightClaims)
		if signals.TotalTerminalSessions == 0 {
			fmt.Printf("[SKIP] item 8 (cache-read rate): tenant %s has no terminal sessions yet to measure\n", tenantID)
		} else {
			check(signals.CacheReadRate >= *cacheReadFloor, "item 8 (cache-read rate): tenant %s measured %.1f%% (floor %.1f%%)", tenantID, signals.CacheReadRate*100, *cacheReadFloor*100)
		}
	}

	// Item 7: evals green in CI — the committed baseline exists and loads;
	// this does NOT re-run the corpus (that's `make eval`'s own job and
	// CI's eval-gate), it only confirms the artifact that job produces is
	// present for this build.
	if baseline, berr := evals.LoadBaseline(filepath.Join("evals", "testdata", "baseline.json")); berr != nil {
		check(false, "item 7 (evals green in CI): %v", berr)
	} else {
		check(len(baseline.Cases) > 0, "item 7 (evals green in CI): committed baseline has %d case(s)", len(baseline.Cases))
	}

	for _, item := range []string{
		"item 3 (sandboxing + human approval for high-impact actions) — code-review property, confirm at review time, not per deployment",
		"item 4 (a lethal-trifecta leg broken per risky flow) — per-flow design review",
		"item 9 (documented data residency/retention/no-train) — policy document outside this codebase",
		"item 10 (rehearsed behavioral-incident runbook) — conducted rehearsal, recorded outside this codebase",
	} {
		fmt.Printf("[MANUAL] %s\n", item)
	}

	if !ok {
		return fmt.Errorf("go-live checklist has at least one FAIL — see docs/go-live.md")
	}
	fmt.Println("go-live: every AUTOMATED item passed (MANUAL items still require sign-off — see above)")
	return nil
}
