package zalo

import (
	"context"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// SealFunc/RunRequest/RunEvent/RunStarter are structurally identical to
// internal/surfaces/rest's own (starter.go) and internal/surfaces/telegram's
// own — duplicated, not imported, per this codebase's established
// cross-surface idiom (outbox.go's own doc comment). This package must
// never import kernel/ directly (tests/contract/boundaries_test.go's
// wildcard rule over internal/surfaces/...).
type SealFunc func(plaintext []byte) (sealed, digest []byte, keyID string, err error)

type RunRequest struct {
	SessionID     uuid.UUID
	TenantID      uuid.UUID
	Seal          SealFunc
	Input         string
	ModelID       string
	AutonomyLevel string
}

type RunEvent struct {
	Event store.Event
	Err   error
}

type RunStarter interface {
	StartRun(ctx context.Context, req RunRequest) (<-chan RunEvent, error)
}
