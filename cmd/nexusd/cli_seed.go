package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	configpg "github.com/truongpx396/nexus-agent-demo/internal/config/postgres"
	costpg "github.com/truongpx396/nexus-agent-demo/internal/cost/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	tenancypg "github.com/truongpx396/nexus-agent-demo/internal/tenancy/postgres"
)

func runSeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	tenantName := fs.String("tenant", "acme", "tenant name to seed")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dsn := envOr("NEXUS_DATABASE_URL", defaultAppDSN)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	// Deterministic from the name, so `make seed TENANT=acme` is idempotent
	// across runs rather than minting a new tenant every time.
	tenantID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("nexus-agent-demo/tenant/"+*tenantName))

	s := store.New(pool)
	err = s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tenancypg.Register(ctx, tx, tenantID, *tenantName)
	})
	if err != nil {
		return fmt.Errorf("seed tenant: %w", err)
	}

	if err := s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return seedPriceBook(ctx, tx, tenantID)
	}); err != nil {
		return fmt.Errorf("seed price book: %w", err)
	}

	if err := s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return seedDemoSkills(ctx, tx, tenantID)
	}); err != nil {
		return fmt.Errorf("seed demo skills: %w", err)
	}

	fmt.Printf("seeded tenant %q (tenant_id=%s)\n", *tenantName, tenantID)
	return nil
}

// demoSkillIDs are the two bundles under skills/ (specs/README.md
// Phase 16, task 16.7) — admitting them at seed time is what makes
// `nexusctl run "research ..."` reach for activate_skill("web-research") on
// a freshly seeded tenant, without a separate manual admission step.
var demoSkillIDs = []string{"web-research", "sandboxed-code"}

// seedDemoSkills admits demoSkillIDs into tenantID's config, UNIONED with
// whatever is already admitted (configpg.Upsert replaces the whole
// admitted_skill_ids array, so a naive overwrite here would silently
// un-admit anything an operator had already turned on by hand) — idempotent
// like seedPriceBook, and harmless if skills/'s bundles fail the signature
// or scan check at load time (task 7.3/7.5): admission and trust are two
// independent gates, checked in two different places.
func seedDemoSkills(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	cfg, err := configpg.Load(ctx, tx, tenantID)
	if err != nil {
		return fmt.Errorf("load tenant config: %w", err)
	}
	admitted := map[string]bool{}
	for _, id := range cfg.AdmittedSkillIDs {
		admitted[id] = true
	}
	changed := false
	for _, id := range demoSkillIDs {
		if !admitted[id] {
			cfg.AdmittedSkillIDs = append(cfg.AdmittedSkillIDs, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	cfg.TenantID = tenantID
	return configpg.Upsert(ctx, tx, cfg)
}

// seedPriceBook inserts the price book entries a tenant is missing, so cost
// governance is never inert (internal/cost.Gate fails closed with "no price
// book entry" on every Reserve, on purpose: an unpriced meter must never look
// free). It is idempotent, and it fills gaps rather than skipping a tenant that
// already has some entries, so switching NEXUS_PROVIDER and seeding again prices
// the new model. See cli_seed_prices.go for which models are priced and why.
func seedPriceBook(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	existing, err := costpg.LoadPriceBook(ctx, tx)
	if err != nil {
		return err
	}
	entries, warning := seedPriceEntries(time.Now())
	if warning != "" {
		fmt.Fprintln(os.Stderr, "seed: "+warning)
	}
	for _, e := range entries {
		if have, ok := existing.Lookup(e.Meter, e.Subject, e.EffectiveFrom); ok && have.Subject == e.Subject {
			continue
		}
		if err := costpg.InsertPriceBookEntry(ctx, tx, tenantID, e); err != nil {
			return err
		}
	}
	return nil
}
