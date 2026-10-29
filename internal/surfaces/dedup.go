package surfaces

import (
	"context"

	"github.com/google/uuid"
)

// InboundDedup is provider-retry dedup (production-readiness review: "no
// inbound dedup on the provider's own delivery ID"). A webhook provider
// redelivers a message it did not see acknowledged; Claim records the
// provider's own delivery id, once, so telegram/zalo/email can acknowledge a
// repeat without dispatching a second turn.
type InboundDedup interface {
	// Claim reports whether (surfaceID, deliveryID) is being seen for the
	// first time, and so should be dispatched. sessionKey is the
	// conversation it belongs to, kept with the claim for diagnosis. An empty
	// deliveryID is a provider that sends none: the adapter treats it as
	// always new rather than collapsing every message into one.
	Claim(ctx context.Context, tenantID uuid.UUID, surfaceID, deliveryID, sessionKey string) (claimed bool, err error)
}
