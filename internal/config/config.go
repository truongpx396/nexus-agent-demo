// Package config is the tenant config store cmd/nexusd's Phase 3 comment
// named as missing ("there is no tenant config store yet — that's Phase 7's
// internal/config"). It holds exactly the two facts Phase 7's memory and
// skills packages need per tenant: which skill bundles a tenant has admitted
// (README task 7.6's "the tenant's admitted set"), and how many days of
// file-first memory stay eligible for session-start injection (task 7.1).
// Config, never forks (pattern #61): a tenant with no row yet is not an
// error, it is the default configuration.
package config

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// DefaultMemoryRetentionDays is what an unconfigured tenant gets — matching
// task 7.1's own "90-day retention" wording.
const DefaultMemoryRetentionDays = 90

// TenantConfig is one tenant's row in migrations/0014_tenant_configs.sql.
type TenantConfig struct {
	TenantID            uuid.UUID
	AdmittedSkillIDs    []string
	MemoryRetentionDays int
}

// defaultFor is what Load returns when tenantID has no row yet.
func defaultFor(tenantID uuid.UUID) TenantConfig {
	return TenantConfig{TenantID: tenantID, MemoryRetentionDays: DefaultMemoryRetentionDays}
}

// Load reads tenantID's config, or the default if it has never been set.
// Callers run this inside Store.InTenantTx like every other tenant-scoped
// read in this codebase — tx is never a bare pool.
func Load(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (TenantConfig, error) {
	var raw []byte
	c := TenantConfig{TenantID: tenantID}
	row := tx.QueryRow(ctx,
		`SELECT admitted_skill_ids, memory_retention_days FROM tenant_configs WHERE tenant_id = $1`,
		tenantID,
	)
	if err := row.Scan(&raw, &c.MemoryRetentionDays); err != nil {
		if err == pgx.ErrNoRows {
			return defaultFor(tenantID), nil
		}
		return TenantConfig{}, fmt.Errorf("config: load %s: %w", tenantID, err)
	}
	if err := json.Unmarshal(raw, &c.AdmittedSkillIDs); err != nil {
		return TenantConfig{}, fmt.Errorf("config: unmarshal admitted_skill_ids for %s: %w", tenantID, err)
	}
	return c, nil
}

// Upsert writes cfg, creating the tenant's row on first use.
func Upsert(ctx context.Context, tx pgx.Tx, cfg TenantConfig) error {
	raw, err := json.Marshal(cfg.AdmittedSkillIDs)
	if err != nil {
		return fmt.Errorf("config: marshal admitted_skill_ids for %s: %w", cfg.TenantID, err)
	}
	retention := cfg.MemoryRetentionDays
	if retention <= 0 {
		retention = DefaultMemoryRetentionDays
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO tenant_configs (tenant_id, admitted_skill_ids, memory_retention_days, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (tenant_id) DO UPDATE
		SET admitted_skill_ids = EXCLUDED.admitted_skill_ids,
		    memory_retention_days = EXCLUDED.memory_retention_days,
		    updated_at = now()`,
		cfg.TenantID, raw, retention,
	)
	if err != nil {
		return fmt.Errorf("config: upsert %s: %w", cfg.TenantID, err)
	}
	return nil
}

// LoadForTenant is Load's convenience wrapper for a caller that only has a
// *store.Store, not an open transaction — cmd/nexusd's skill/memory wiring
// is the caller, mirroring internal/memory.Store.LoadForSession's identical
// shape.
func LoadForTenant(ctx context.Context, st *store.Store, tenantID uuid.UUID) (TenantConfig, error) {
	var cfg TenantConfig
	err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cfg, err = Load(ctx, tx, tenantID)
		return err
	})
	return cfg, err
}

// HasSkill reports whether skillID is in cfg's admitted set.
func (c TenantConfig) HasSkill(skillID string) bool {
	for _, id := range c.AdmittedSkillIDs {
		if id == skillID {
			return true
		}
	}
	return false
}
