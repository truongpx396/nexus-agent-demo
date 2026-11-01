// Package postgres is the Postgres implementation of config.Loader, plus the
// transaction-level Load and Upsert that cmd/nexusd's seed command composes
// with other writes in one tenant-scoped transaction.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/config"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Store is config.Loader over Postgres.
type Store struct {
	st *store.Store
}

// NewStore returns a Store over st.
func NewStore(st *store.Store) *Store { return &Store{st: st} }

var _ config.Loader = (*Store)(nil)

// Load reads tenantID's config, or the default if it has never been set.
func (s *Store) Load(ctx context.Context, tenantID uuid.UUID) (config.TenantConfig, error) {
	var cfg config.TenantConfig
	err := s.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cfg, err = Load(ctx, tx, tenantID)
		return err
	})
	return cfg, err
}

// Load reads tenantID's config inside tx, or the default if it has never been
// set. Callers run this inside Store.InTenantTx like every other tenant-scoped
// read in this codebase — tx is never a bare pool.
func Load(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (config.TenantConfig, error) {
	var rawSkills, rawProviders []byte
	c := config.TenantConfig{TenantID: tenantID}
	row := tx.QueryRow(ctx,
		`SELECT admitted_skill_ids, memory_retention_days, admitted_connector_providers FROM tenant_configs WHERE tenant_id = $1`,
		tenantID,
	)
	if err := row.Scan(&rawSkills, &c.MemoryRetentionDays, &rawProviders); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return config.Default(tenantID), nil
		}
		return config.TenantConfig{}, fmt.Errorf("config: load %s: %w", tenantID, err)
	}
	if err := json.Unmarshal(rawSkills, &c.AdmittedSkillIDs); err != nil {
		return config.TenantConfig{}, fmt.Errorf("config: unmarshal admitted_skill_ids for %s: %w", tenantID, err)
	}
	if err := json.Unmarshal(rawProviders, &c.AdmittedConnectorProviders); err != nil {
		return config.TenantConfig{}, fmt.Errorf("config: unmarshal admitted_connector_providers for %s: %w", tenantID, err)
	}
	return c, nil
}

// Upsert writes cfg inside tx, creating the tenant's row on first use.
func Upsert(ctx context.Context, tx pgx.Tx, cfg config.TenantConfig) error {
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
		retention = config.DefaultMemoryRetentionDays
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
