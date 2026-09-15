package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/nexus-agent-demo/internal/config"
	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
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
		_, err := tx.Exec(ctx,
			`INSERT INTO tenants (tenant_id, name) VALUES ($1, $2)
			 ON CONFLICT (tenant_id) DO NOTHING`,
			tenantID, *tenantName,
		)
		return err
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

// demoSkillIDs are the two bundles under skills/ (docs/build-phases.md
// Phase 16, task 16.7) — admitting them at seed time is what makes
// `nexusctl run "research ..."` reach for activate_skill("web-research") on
// a freshly seeded tenant, without a separate manual admission step.
var demoSkillIDs = []string{"web-research", "sandboxed-code"}

// seedDemoSkills admits demoSkillIDs into tenantID's config, UNIONED with
// whatever is already admitted (config.Upsert replaces the whole
// admitted_skill_ids array, so a naive overwrite here would silently
// un-admit anything an operator had already turned on by hand) — idempotent
// like seedPriceBook, and harmless if skills/'s bundles fail the signature
// or scan check at load time (task 7.3/7.5): admission and trust are two
// independent gates, checked in two different places.
func seedDemoSkills(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	cfg, err := config.Load(ctx, tx, tenantID)
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
	return config.Upsert(ctx, tx, cfg)
}

// seedPriceBook inserts one price book entry per (meter, model) pair the
// first time a tenant is seeded — cost governance is otherwise inert
// (internal/cost.Gate fails closed with "no price book entry" on every
// Reserve, on purpose: an unpriced meter must never look free). Idempotent
// like the tenant insert above it: a second `make seed` for the same
// tenant is a no-op once claude-sonnet-5's entries already exist.
//
// README task 13.6 (closing production-readiness finding F8): every entry
// used to share cost.WildcardSubject at one Sonnet-class rate, while
// internal/provider/router.go actually routes across three models whose
// real per-token rates differ by 5x — confidential/complex traffic routed
// to Opus was billed at 60% of true cost, public/simple traffic routed to
// Haiku at 3x. Real rates below (per README task 13.6's own note, confirmed
// current Anthropic pricing): cache write priced at 1.25x that model's own
// input rate, cache read at 0.1x — the standard Anthropic multipliers,
// applied per model rather than carried over from one wildcard figure.
// MeterEmbeddingTokens keeps its one WildcardSubject entry: this demo's
// retrieval tier always embeds under "fake-embedder-v1" (README task 12.5),
// never one of the three routed chat models, so there is no per-model tier
// to seed for it.
func seedPriceBook(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	existing, err := cost.LoadPriceBook(ctx, tx)
	if err != nil {
		return err
	}
	if _, ok := existing.Lookup(cost.MeterOutput, "claude-sonnet-5", time.Now()); ok {
		return nil
	}

	now := time.Now()
	for _, m := range []struct {
		model                  string
		inputPerMillionMicros  int64
		outputPerMillionMicros int64
	}{
		{"claude-haiku-4-5", 1_000_000, 5_000_000}, // $1.00 in / $5.00 out per million tokens
		{"claude-sonnet-5", 2_000_000, 10_000_000}, // $2.00 in / $10.00 out per million tokens
		{"claude-opus-5", 5_000_000, 25_000_000},   // $5.00 in / $25.00 out per million tokens
	} {
		for _, e := range []struct {
			meter cost.MeterID
			price int64
		}{
			{cost.MeterInputUncached, m.inputPerMillionMicros},
			{cost.MeterInputCacheWrite, m.inputPerMillionMicros * 5 / 4}, // 1.25x the model's own input rate
			{cost.MeterInputCacheRead, m.inputPerMillionMicros / 10},     // 0.1x the model's own input rate
			{cost.MeterOutput, m.outputPerMillionMicros},
		} {
			if err := cost.InsertPriceBookEntry(ctx, tx, tenantID, cost.PriceBookEntry{
				Meter: e.meter, Subject: m.model, Version: 1,
				Currency: cost.DefaultCurrency, PricePerMillionMicros: e.price, EffectiveFrom: now,
			}); err != nil {
				return err
			}
		}
	}

	return cost.InsertPriceBookEntry(ctx, tx, tenantID, cost.PriceBookEntry{
		Meter: cost.MeterEmbeddingTokens, Subject: cost.WildcardSubject, Version: 1,
		Currency: cost.DefaultCurrency, PricePerMillionMicros: 100_000, EffectiveFrom: now, // $0.10 / million embedding tokens
	})
}
