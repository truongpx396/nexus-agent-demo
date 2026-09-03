package telegram

import (
	"context"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// SealFunc/RunRequest/RunEvent/RunStarter are structurally identical to
// internal/surfaces/rest's own (starter.go) — duplicated, not imported,
// per this codebase's established cross-surface idiom (outbox.go's own doc
// comment: internal/surfaces/rest and internal/surfaces/telegram share no
// direct dependency, only cmd/nexusd knows both exist). This package must
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

// RunStarter is the entire seam between this surface and whatever actually
// executes a run — cmd/nexusd supplies the concrete implementation backed
// by kernel.Kernel, the same kernelRunStarter every other surface shares.
type RunStarter interface {
	StartRun(ctx context.Context, req RunRequest) (<-chan RunEvent, error)
}
