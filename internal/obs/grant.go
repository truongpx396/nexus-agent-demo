package obs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
)

// Grant mirrors one content_access_grants row (migrations/
// 0009_content_access_grants.sql): the only path to plaintext outside a
// run's own audience (README task 5.11, pattern 35). This is deliberately
// distinct from internal/surfaces/rest/server.go's existing owner-decrypt
// path (handleEvents, gated on sess.UserID == userID — pattern #51,
// already shipped): that path is the run's OWN audience reading its own
// content; a Grant authorizes someone ELSE — an operator/support
// principal — for a bounded, audited, expiring window.
type Grant struct {
	GrantID   uuid.UUID
	TenantID  uuid.UUID
	SessionID uuid.UUID
	GranteeID uuid.UUID
	Reason    string
	ExpiresAt time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

func (g Grant) valid(at time.Time) bool {
	return g.RevokedAt == nil && at.Before(g.ExpiresAt)
}

// EventDTO is the decrypted shape Read returns — deliberately narrow (no
// Payload, no crypto details), the same information REST's own eventDTO
// exposes to a run's own audience.
type EventDTO struct {
	EventID   uuid.UUID
	Seq       int64
	Type      eventlog.EventType
	Body      json.RawMessage
	CreatedAt time.Time
}

// GrantStore is everything Grants needs from storage, and nothing about how
// it is kept: the grant rows, the audit events about them (each hash-chained
// by the adapter), and the decrypted history a grant unlocks. The Postgres
// adapter is internal/obs/postgres; every method runs in the tenant's own
// scope.
type GrantStore interface {
	// Issue stores gr (setting its CreatedAt) and appends the
	// content_access_granted event for it, atomically: a grant exists if and
	// only if it was audited.
	Issue(ctx context.Context, gr Grant) (Grant, error)

	// ListFor returns every grant for (sessionID, granteeID), newest first,
	// expired and revoked ones included — deciding which is currently valid
	// is Grants' job, not storage's.
	ListFor(ctx context.Context, tenantID, sessionID, granteeID uuid.UUID) ([]Grant, error)

	// RecordRefusal appends content_access_refused: a DENIED attempt gets its
	// own audit trail, not just a granted one.
	RecordRefusal(ctx context.Context, tenantID, sessionID, granteeID uuid.UUID, reason string) error

	// History returns the session's events in order with their payloads
	// decrypted. It never opens an EventThought, even under a grant: a
	// thought comes back with no Body at all.
	History(ctx context.Context, tenantID, sessionID uuid.UUID) ([]EventDTO, error)

	// RecordAccess appends content_accessed: ONE receipt per read, however
	// many events that read returned.
	RecordAccess(ctx context.Context, tenantID, sessionID, granteeID uuid.UUID, eventCount int) error
}

// Grants issues and enforces content access grants. Every grant AND every
// read under it produces its own hash-chained audit receipt
// (content_access_granted, content_accessed) — "audited, expiring, receipt
// per read" (task 5.11's own wording).
type Grants struct {
	Store GrantStore

	// Now is the clock; nil means time.Now. Grants are time-bounded, so a
	// test needs to be able to move it.
	Now func() time.Time
}

// NewGrants returns Grants over store.
func NewGrants(store GrantStore) *Grants { return &Grants{Store: store} }

func (g *Grants) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// RequestGrant issues a new grant and appends EventContentAccessGranted. A
// non-positive ttl means an hour.
func (g *Grants) RequestGrant(ctx context.Context, tenantID, sessionID, granteeID uuid.UUID, reason string, ttl time.Duration) (Grant, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	gr := Grant{GrantID: uuid.New(), TenantID: tenantID, SessionID: sessionID, GranteeID: granteeID, Reason: reason, ExpiresAt: g.now().Add(ttl)}
	return g.Store.Issue(ctx, gr)
}

// Authorize finds a currently-valid grant for (sessionID, granteeID) —
// unexpired and unrevoked. Refuses (and appends EventContentAccessRefused,
// its own audit trail for a DENIED attempt, not just a granted one) when
// none exists.
func (g *Grants) Authorize(ctx context.Context, tenantID, sessionID, granteeID uuid.UUID) (Grant, error) {
	candidates, err := g.Store.ListFor(ctx, tenantID, sessionID, granteeID)
	if err != nil {
		return Grant{}, err
	}
	now := g.now()
	for _, c := range candidates {
		if c.valid(now) {
			return c, nil
		}
	}
	if err := g.Store.RecordRefusal(ctx, tenantID, sessionID, granteeID, "no valid grant"); err != nil {
		return Grant{}, err
	}
	return Grant{}, fmt.Errorf("obs: no valid content access grant for session %s, grantee %s", sessionID, granteeID)
}

// Read authorizes granteeID against sessionID (Authorize) and, only on
// success, returns every event's decrypted body — recording ONE
// EventContentAccessed receipt for this read (task 5.11: "receipt ... on
// every read," read here meaning every access this method grants, not one
// receipt per individual event it happens to decrypt). If the receipt cannot
// be recorded the content is withheld: no unaudited read.
func (g *Grants) Read(ctx context.Context, tenantID, sessionID, granteeID uuid.UUID) ([]EventDTO, error) {
	if _, err := g.Authorize(ctx, tenantID, sessionID, granteeID); err != nil {
		return nil, err
	}
	history, err := g.Store.History(ctx, tenantID, sessionID)
	if err != nil {
		return nil, err
	}
	if err := g.Store.RecordAccess(ctx, tenantID, sessionID, granteeID, len(history)); err != nil {
		return nil, fmt.Errorf("obs: record content access receipt: %w", err)
	}
	return history, nil
}
