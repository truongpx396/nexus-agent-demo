// Package tenancy is the platform operator's view of the tenants themselves.
// Everything else in the system is scoped to one tenant for the life of a
// transaction; the few jobs that must visit every tenant (the audit anchor
// pass, the team and idle-conversation backstops) need a way to say which ones
// exist, and this is it. The tenants table is owned here, not by whichever
// binary happens to run those jobs. The Postgres adapter is
// internal/tenancy/postgres.
package tenancy

import (
	"context"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Directory lists the tenants. It is the one genuinely cross-tenant read in the
// platform: unlike every tenant-scoped store, an implementation must act as the
// platform operator, not as a tenant.
type Directory interface {
	ListTenantIDs(ctx context.Context) ([]uuid.UUID, error)
}

// ForEach runs fn once per tenant in dir, in listing order. It is the shape
// every periodic cross-tenant pass shares: a failure to list the tenants is
// logged and ends the pass, and a failure for one tenant is logged and does not
// stop the others, so one broken tenant cannot starve the rest. name labels the
// pass in the log.
func ForEach(ctx context.Context, dir Directory, name string, fn func(ctx context.Context, tenantID uuid.UUID) error) {
	ids, err := dir.ListTenantIDs(ctx)
	if err != nil {
		log.Error().Err(err).Str("pass", name).Msg("tenancy: list tenants failed")
		return
	}
	for _, id := range ids {
		if err := fn(ctx, id); err != nil {
			log.Error().Err(err).Str("pass", name).Any("tenant_id", id).Msg("tenancy: pass failed for tenant")
		}
	}
}
