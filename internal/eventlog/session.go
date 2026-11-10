package eventlog

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrSessionNotFound is what a session read wraps when no row is visible: the
// session does not exist, or it belongs to another tenant (RLS cannot tell the
// two apart, and neither should the caller). It lives here, in the shared
// vocabulary, so a feature can recognise it without importing the persistence
// kit that produces it.
var ErrSessionNotFound = errors.New("session not found")

// Session is the subset of the sessions table (migration 0002) Phase 2
// reads and writes. Columns this layer never touches (audience_ref,
// execution_class, priority, region, ...) keep their schema defaults —
// they're seams for what owns them, not dead weight to model here.
//
// Status and TerminalReason are marked PROJECTION in the schema comment, but
// Phase 2 writes them directly (UpdateSessionStatus, in the same transaction
// as the EventTerminal append that changes them) rather than deriving them
// by replaying the log on every read: the log is still the only source of
// truth (an event log replay from empty events would be undefined, not
// stale), but replay-on-read would need to decrypt every event's Payload
// just to answer "is this run done yet" — these two columns exist in the
// schema precisely so that question doesn't require a decrypt. Never written
// independently of the event that justifies the change is what keeps this
// from becoming a second source of truth.
type Session struct {
	SessionID      uuid.UUID
	SessionKey     string
	TenantID       uuid.UUID
	SurfaceID      string
	UserID         uuid.UUID
	AgentID        uuid.UUID
	AgentVersion   int
	HarnessDigest  []byte
	DataLabel      string
	RouteModelID   string
	RouteReason    map[string]string
	AutonomyLevel  string
	Status         string
	TerminalReason *string

	// ParentSessionID is the session that spawned this one: nil for a thread's
	// own top-level session, set by whoever creates a session on another's behalf
	// (a delegation, a team member). It is the one fact about the spawn tree the
	// aggregate keeps, because "which sessions are top-level threads" is a
	// question about sessions; how deep a session sits and which root it descends
	// from are the spawning features' (internal/delegate's session_lineage).
	ParentSessionID *uuid.UUID

	// CreatedAt is read-only (the column's own DEFAULT now() -- CreateSession
	// never sets it). Added for ListSessionsForUser's own sort/display need;
	// GetSession fills it too since it's cheap to select alongside every
	// other column already read there.
	CreatedAt time.Time

	// Conversational opts this session into kernel.RunConfig.Conversational
	// pause-not-terminate semantics (migrations/0023_conversational_
	// sessions.sql) -- pinned at creation, exactly like AutonomyLevel, and
	// read back on every internal/runctl.Control.ResumeConversation call to
	// rebuild RunConfig. False for every session before this migration.
	Conversational bool

	// UpdatedAt is written by UpdateSessionStatus on every status
	// transition -- the idle-conversation sweep's own signal
	// (cmd/nexusd/background.go) for how long a session has sat in
	// SessionStatusAwaitingInput with nobody answering. Read-only here
	// (CreateSession never sets it; the column's own DEFAULT now() does).
	UpdatedAt time.Time
}

// SessionRows reads and writes session rows and their history inside one
// tenant-scoped unit of work. A service's own Tx interface embeds it, so a
// session's status never changes in a different transaction from the event that
// justifies it. The Postgres implementation is internal/eventlog/sealed.
type SessionRows interface {
	// GetSession returns the session, or an error if it does not exist in the
	// tenant's scope (which cannot be told apart from another tenant's).
	GetSession(ctx context.Context, sessionID uuid.UUID) (Session, error)

	// CreateSession inserts a new session row.
	CreateSession(ctx context.Context, s Session) error

	// SetSessionStatus moves the session to status, recording terminalReason
	// when it ends.
	SetSessionStatus(ctx context.Context, sessionID uuid.UUID, status string, terminalReason *string) error

	// ListEvents returns the session's events in sequence order, still sealed.
	ListEvents(ctx context.Context, sessionID uuid.UUID) ([]Event, error)
}
