package teams

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/cost"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// ErrNotFound mirrors internal/delegate.ErrNotFound's own convention: "it
// never existed" and "RLS made a cross-tenant row invisible" are
// indistinguishable on purpose.
var ErrNotFound = errors.New("teams: not found")

// Tx is one open unit of work in a single tenant's scope: everything creating,
// running and ending a team reads and changes, so that a team's status, its
// board, the member sessions and the events appended to the coordinator commit
// together or not at all. The Postgres implementation is
// internal/teams/postgres, a pgx transaction under the tenant's row-level
// security; the rules — the depth bound, when a team is complete, what ending
// one records — are this package's.
type Tx interface {
	eventlog.Appender
	eventlog.SessionRows
	crypto.Keys

	// InsertTeam creates an 'active' team. The roster is fixed here and never
	// updated (task 9.1's "no mid-run recruitment").
	InsertTeam(ctx context.Context, teamID, tenantID uuid.UUID, name string, coordinatorSessionID uuid.UUID, roster []MemberSpec) error
	GetTeam(ctx context.Context, teamID uuid.UUID) (Team, error)
	SetTeamEnvelope(ctx context.Context, teamID, envelopeID uuid.UUID) error

	// EndTeamIfActive atomically moves a team out of 'active' and reports whether
	// this caller did: a second, concurrent caller finds ok=false and nothing to
	// do. It returns the coordinator session the team_ended event belongs to.
	EndTeamIfActive(ctx context.Context, teamID uuid.UUID, status Status, reason string) (coordinatorSessionID uuid.UUID, ok bool, err error)

	// BoardAndMemberCounts is how many cards are still open or claimed and how
	// many member sessions have not reached a terminal status.
	BoardAndMemberCounts(ctx context.Context, teamID uuid.UUID) (openOrClaimed, activeMembers int, err error)
	ListActiveMemberSessionIDs(ctx context.Context, teamID uuid.UUID) ([]uuid.UUID, error)
	ListStaleActiveTeamIDs(ctx context.Context, tenantID uuid.UUID, olderThan time.Time) ([]uuid.UUID, error)
	TeamIDForSession(ctx context.Context, sessionID uuid.UUID) (teamID uuid.UUID, ok bool, err error)

	// The board.
	InsertCard(ctx context.Context, tenantID, teamID uuid.UUID, title, body string, writtenBy uuid.UUID, taint [3]bool, scan ScanStatus, findings []string) (Card, error)
	ListCards(ctx context.Context, teamID uuid.UUID) ([]Card, error)
	// ClaimCard atomically claims one open, clean card; claimed=false means it
	// was not available (already claimed, flagged, or not on this team's board).
	ClaimCard(ctx context.Context, tenantID, teamID, cardID, sessionID uuid.UUID) (card Card, claimed bool, err error)
	// UpdateCardStatus moves a card, but only for the session holding its claim;
	// ok=false is "not claimed by you".
	UpdateCardStatus(ctx context.Context, teamID, cardID, claimantSessionID uuid.UUID, status CardStatus) (Card, bool, error)

	// The team's shared budget envelope.
	CreateEnvelope(ctx context.Context, tenantID, teamID uuid.UUID, ceiling cost.Money, memberCount int) (uuid.UUID, error)
	// DrawFromEnvelope atomically takes amount, refusing (ok=false, with what is
	// left) rather than going negative; an envelope that does not exist is an
	// error.
	DrawFromEnvelope(ctx context.Context, envelopeID uuid.UUID, amount cost.Money) (ok bool, remaining cost.Money, err error)
	ReturnToEnvelope(ctx context.Context, envelopeID uuid.UUID, amount cost.Money) error
	EnvelopeShare(ctx context.Context, envelopeID uuid.UUID) (ceiling cost.Money, memberCount int, err error)
}

// Store runs a unit of work in a tenant's scope: it opens the unit, calls fn
// with a Tx bound to it, and commits if fn returns nil or rolls back if not.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx Tx) error) error
}

// PriceBooks is the part of cost.Ledger an envelope needs to price a member's
// real usage.
type PriceBooks interface {
	LoadPriceBook(ctx context.Context, tenantID uuid.UUID) (*cost.PriceBook, error)
}
