package runs

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// ErrNotFound reports a run that does not exist or is not visible to the
// tenant asking. A reader must not distinguish the two: telling a tenant that
// another tenant's run exists is itself a leak.
var ErrNotFound = errors.New("run not found")

// RunView is a run as the read side shows it: the facts a client may display
// about a session, and nothing of its sealed content.
type RunView struct {
	RunID          uuid.UUID
	UserID         uuid.UUID
	Status         string
	TerminalReason *string
	CreatedAt      time.Time
	AutonomyLevel  string
}

// Reader is the read side of runs: what a surface needs to show a run back to
// its own audience. Every method is scoped to one tenant.
type Reader interface {
	// GetRun returns one run, or ErrNotFound.
	GetRun(ctx context.Context, tenantID, runID uuid.UUID) (RunView, error)

	// ListRuns returns userID's own root runs, newest first, at most limit.
	ListRuns(ctx context.Context, tenantID, userID uuid.UUID, limit int) ([]RunView, error)

	// Events returns runID's events in sequence order, still sealed.
	Events(ctx context.Context, tenantID, runID uuid.UUID) ([]eventlog.Event, error)

	// EventOpener returns a decryptor for tenantID's events. It is meant for
	// one request: it may cache the keys it unwraps, so a caller must not
	// hold on to it.
	EventOpener(tenantID uuid.UUID) EventOpener
}

// EventOpener decrypts event payloads for display to a run's own audience.
type EventOpener interface {
	// Open returns e's plaintext payload.
	Open(ctx context.Context, e eventlog.Event) ([]byte, error)
}
