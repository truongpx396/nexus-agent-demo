// Package postgres is the Postgres adapter for the surfaces' shared
// persistence ports: inbound-delivery dedup and the delivery outbox.
package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces"
)

// Dedup implements surfaces.InboundDedup over store.ClaimInboundDelivery, in
// the tenant's own scope.
type Dedup struct {
	st *store.Store
}

var _ surfaces.InboundDedup = (*Dedup)(nil)

// NewDedup returns a Dedup that opens its tenant-scoped transactions on st.
func NewDedup(st *store.Store) *Dedup { return &Dedup{st: st} }

func (d *Dedup) Claim(ctx context.Context, tenantID uuid.UUID, surfaceID, deliveryID, sessionKey string) (claimed bool, err error) {
	err = d.st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		claimed, derr = store.ClaimInboundDelivery(ctx, tx, tenantID, surfaceID, deliveryID, sessionKey)
		return derr
	})
	return claimed, err
}
