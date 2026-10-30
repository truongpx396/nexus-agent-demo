package oversight

import (
	"context"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// Tx is one open unit of work in a single tenant's scope: everything an
// approval or an input request needs to read, change and record, so that a
// decision and the event that records it commit together or not at all. The
// Postgres implementation (internal/oversight/postgres) is a pgx transaction
// under the tenant's row-level security; the business rules — what may be
// decided, when a request has expired, what each outcome appends — are the
// closures in approval.go, input.go and invalidate.go that receive it.
//
// The "Lock" reads are SELECT … FOR UPDATE in Postgres: two people deciding
// the same approval at once must serialize, and the second must see it
// already decided.
type Tx interface {
	eventlog.Appender

	// Approvals.
	InsertApproval(ctx context.Context, ap *Approval) error // sets ap.CreatedAt
	GetApproval(ctx context.Context, approvalID uuid.UUID) (Approval, error)
	LockApproval(ctx context.Context, approvalID uuid.UUID) (Approval, error)
	PendingApprovals(ctx context.Context) ([]Approval, error)
	// LockPendingApproval is the one approval a session has outstanding;
	// found=false means there is none (not an error).
	LockPendingApproval(ctx context.Context, sessionID uuid.UUID) (ap Approval, found bool, err error)
	SetApprovalStatus(ctx context.Context, approvalID uuid.UUID, status ApprovalStatus, decidedBy, reason string) error
	// SetApprovalModified records a grant with a substituted input and rebinds
	// the approval's canonical digest to it.
	SetApprovalModified(ctx context.Context, approvalID uuid.UUID, decidedBy string, grantedInput, newDigest []byte) error
	// SessionOwner is the user a session belongs to — the default assignee.
	SessionOwner(ctx context.Context, sessionID uuid.UUID) (uuid.UUID, error)

	// Input requests.
	InsertInputRequest(ctx context.Context, ir *InputRequest) error // sets ir.CreatedAt
	LockInputRequest(ctx context.Context, inputRequestID uuid.UUID) (InputRequest, error)
	LockPendingInputRequests(ctx context.Context, sessionID uuid.UUID) ([]InputRequest, error)
	SetInputAnswered(ctx context.Context, inputRequestID uuid.UUID, answer []byte, usedDefault bool) error
	SetInputStatus(ctx context.Context, inputRequestID uuid.UUID, status InputStatus, reason string) error
}

// Store runs a unit of work in a tenant's scope: it opens the unit, calls fn
// with a Tx bound to it, and commits if fn returns nil or rolls back if not.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx Tx) error) error
}

// SessionInfo is the part of a session the Resumer needs to rebuild a run.
type SessionInfo struct {
	AutonomyLevel string
}

// Sessions loads what a suspended run is rebuilt from: its session and its
// full, still-sealed history.
type Sessions interface {
	Load(ctx context.Context, tenantID, sessionID uuid.UUID) (SessionInfo, []eventlog.Event, error)
}

// KeyOpener unwraps a tenant's data key, so the history can be decrypted and
// the resumed run can seal new events under the session's own key.
type KeyOpener interface {
	Unwrap(ctx context.Context, tenantID uuid.UUID, keyID string) (crypto.DEK, error)
}
