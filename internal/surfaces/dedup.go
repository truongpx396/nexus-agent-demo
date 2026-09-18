package surfaces

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// ClaimDelivery wraps store.ClaimInboundDelivery in its own transaction —
// the same "surface calls a store function inside InTenantTx" shape
// GetSessionByKey/CreateSession already establish per dispatch() in every
// webhook surface — so telegram/zalo/email each get provider-retry dedup
// (production-readiness review: "no inbound dedup on the provider's own
// delivery ID") for one function call, with no new interface or wiring of
// their own to carry: every surface already holds a *store.Store.
func ClaimDelivery(ctx context.Context, st *store.Store, tenantID uuid.UUID, surfaceID, deliveryID, sessionKey string) (claimed bool, err error) {
	err = st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		claimed, derr = store.ClaimInboundDelivery(ctx, tx, tenantID, surfaceID, deliveryID, sessionKey)
		return derr
	})
	return claimed, err
}
