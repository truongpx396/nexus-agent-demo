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

	"github.com/google/uuid"
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

// Default is what a Loader returns for a tenant that has never set its config.
func Default(tenantID uuid.UUID) TenantConfig {
	return TenantConfig{TenantID: tenantID, MemoryRetentionDays: DefaultMemoryRetentionDays}
}

// Loader reads a tenant's config in that tenant's scope. A tenant with no row
// yet is not an error: it is Default. The Postgres implementation is
// internal/config/postgres.
type Loader interface {
	Load(ctx context.Context, tenantID uuid.UUID) (TenantConfig, error)
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
