package rest

import (
	"context"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// SealFunc seals one event's plaintext payload — the same shape
// kernel.SealFunc declares, duplicated here rather than imported: this
// package must never import kernel/ directly (README.md §4's architecture
// has a surface reach the kernel through the control plane, not directly;
// tests/contract/boundaries_test.go's "surfaces must not import the kernel
// directly" rule enforces it). cmd/nexusd's RunStarter implementation is
// free to convert between the two — they're structurally identical function
// types, and Go allows that conversion without either package knowing about
// the other's declaration.
type SealFunc func(plaintext []byte) (sealed, digest []byte, keyID string, err error)

// RunRequest is everything StartRun needs to begin one run. The session row
// has already been durably created (handleCreateRun) before this is called.
type RunRequest struct {
	SessionID uuid.UUID
	TenantID  uuid.UUID
	Seal      SealFunc
	Input     string
	ModelID   string
}

// RunEvent is one item from the channel StartRun returns — store.Event
// mirrors kernel.Kernel.Run's iter.Seq2[store.Event, error] shape exactly,
// without this package needing to name the kernel package to say so.
type RunEvent struct {
	Event store.Event
	Err   error
}

// RunStarter is the entire seam between this surface and whatever actually
// executes a run. cmd/nexusd supplies the concrete implementation, backed by
// kernel.Kernel — the only place in the binary that is allowed to import
// both this package and kernel/.
type RunStarter interface {
	// StartRun begins a run in the background (a real implementation
	// returns once accepted, not once the run completes) and returns a
	// channel of every event the run produces, in order, closed when the
	// run ends.
	StartRun(ctx context.Context, req RunRequest) (<-chan RunEvent, error)
}
