package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/nexus-agent-demo/internal/audit"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

func runMigrate(ctx context.Context) error {
	dsn := envOr("NEXUS_MIGRATE_DATABASE_URL", defaultMigrateDSN)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	applied, err := store.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	for _, a := range applied {
		fmt.Printf("applied %s\n", a.Version)
	}

	enabled, total, err := store.RLSTableCount(ctx, pool)
	if err != nil {
		return fmt.Errorf("count RLS tables: %w", err)
	}
	fmt.Printf("RLS enabled on %d/%d tenant tables; tenant scope is transaction-local\n", enabled, total)
	return nil
}

// runVerifyChain is `nexusd verify-chain` (the Makefile's own stubbed
// target, README task 5.3): anchor and verify every tenant's audit chain
// once, printing a clean/broken report per tenant instead of the periodic
// background pass startAnchorLoop runs inside `serve`.
func runVerifyChain(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify-chain", flag.ExitOnError)
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

	signer := audit.NewSignerClient(envOr("NEXUS_SIGNERD_SOCKET", defaultSignerdSocket))
	chain := audit.NewChain(signer)

	tenantIDs, err := listTenantIDs(ctx)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}

	anyBroken := false
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
		if err != nil {
			return fmt.Errorf("verify tenant %s: %w", tenantID, err)
		}
		if report.OK() {
			fmt.Printf("tenant %s: OK (%d receipts, %d anchors)\n", tenantID, report.ReceiptsChecked, report.AnchorsChecked)
			continue
		}
		anyBroken = true
		fmt.Printf("tenant %s: BROKEN — %d break(s), %d gap(s)\n", tenantID, len(report.Breaks), len(report.Gaps))
		for _, b := range report.Breaks {
			fmt.Printf("  break: session=%s seq=%d kind=%s detail=%s\n", b.SessionID, b.Seq, b.Kind, b.Detail)
		}
		for _, g := range report.Gaps {
			fmt.Printf("  gap: session=%s missing_seq=%d\n", g.SessionID, g.MissingSeq)
		}
	}
	if anyBroken {
		return fmt.Errorf("audit chain verification found a break or gap")
	}
	return nil
}
