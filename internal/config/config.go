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

	// AdmittedConnectorProviders is Phase 11's own admitted-set (README task
	// 11.2, migrations/0018_oauth_connections.sql): the vetted, per-tenant
	// OAuth provider allowlist docs/constitution.md requires ("Connectors
	// MUST attach only through the vetted, per-tenant ... catalog"). A
	// tenant with no row admits none — connectors are opt-in, the same
	// "absent means defaults" rule AdmittedSkillIDs already follows, just
	// with an empty default instead of a populated one.
	AdmittedConnectorProviders []string
}

// defaultFor is what Load returns when tenantID has no row yet.
func defaultFor(tenantID uuid.UUID) TenantConfig {
	return TenantConfig{TenantID: tenantID, MemoryRetentionDays: DefaultMemoryRetentionDays}
}

// Load reads tenantID's config, or the default if it has never been set.
// Callers run this inside Store.InTenantTx like every other tenant-scoped
// read in this codebase — tx is never a bare pool.
func Load(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (TenantConfig, error) {
	var rawSkills, rawProviders []byte
	c := TenantConfig{TenantID: tenantID}
	row := tx.QueryRow(ctx,
		`SELECT admitted_skill_ids, memory_retention_days, admitted_connector_providers FROM tenant_configs WHERE tenant_id = $1`,
		tenantID,
	)
	if err := row.Scan(&rawSkills, &c.MemoryRetentionDays, &rawProviders); err != nil {
		if err == pgx.ErrNoRows {
			return defaultFor(tenantID), nil
		}
		return TenantConfig{}, fmt.Errorf("config: load %s: %w", tenantID, err)
	}
	if err := json.Unmarshal(rawSkills, &c.AdmittedSkillIDs); err != nil {
		return TenantConfig{}, fmt.Errorf("config: unmarshal admitted_skill_ids for %s: %w", tenantID, err)
	}
	if err := json.Unmarshal(rawProviders, &c.AdmittedConnectorProviders); err != nil {
		return TenantConfig{}, fmt.Errorf("config: unmarshal admitted_connector_providers for %s: %w", tenantID, err)
	}
	return c, nil
}

// Upsert writes cfg, creating the tenant's row on first use.
func Upsert(ctx context.Context, tx pgx.Tx, cfg TenantConfig) error {
	rawSkills, err := json.Marshal(cfg.AdmittedSkillIDs)
	if err != nil {
		return fmt.Errorf("config: marshal admitted_skill_ids for %s: %w", cfg.TenantID, err)
	}
	rawProviders, err := json.Marshal(cfg.AdmittedConnectorProviders)
	if err != nil {
		return fmt.Errorf("config: marshal admitted_connector_providers for %s: %w", cfg.TenantID, err)
	}
	retention := cfg.MemoryRetentionDays
	if retention <= 0 {
		retention = DefaultMemoryRetentionDays
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO tenant_configs (tenant_id, admitted_skill_ids, memory_retention_days, admitted_connector_providers, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (tenant_id) DO UPDATE
		SET admitted_skill_ids = EXCLUDED.admitted_skill_ids,
		    memory_retention_days = EXCLUDED.memory_retention_days,
		    admitted_connector_providers = EXCLUDED.admitted_connector_providers,
		    updated_at = now()`,
		cfg.TenantID, rawSkills, retention, rawProviders,
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

// HasConnectorProvider reports whether provider is in cfg's admitted
// connector-provider set (README task 11.2) — internal/connectors.BeginAuth
// checks this before ever building an authorization-code redirect URL, the
// same fail-closed shape HasSkill already gives task 7.4's intersection
// check.
func (c TenantConfig) HasConnectorProvider(provider string) bool {
	for _, p := range c.AdmittedConnectorProviders {
		if p == provider {
			return true
		}
	}
	return false
}
