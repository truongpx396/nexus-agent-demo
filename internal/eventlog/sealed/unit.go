package sealed

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Unit is the part of a service's tenant-scoped unit of work that every
// out-of-band service needs the same way: append a sealed event, read and write
// a session, mint and open a data key — all in one pgx transaction. A service's
// Postgres Tx embeds *Unit and adds the queries of its own tables, reaching the
// transaction through Tx().
type Unit struct {
	eventlog.Appender
	app  sealedAppender
	tx   pgx.Tx
	keys keyStore
}

// keyStore is the part of crypto/postgres.KeyStore a Unit uses.
type keyStore interface {
	NewDEK(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (crypto.DEK, error)
	Unwrap(ctx context.Context, tx pgx.Tx, keyID string) (crypto.DEK, error)
}

var (
	_ eventlog.Appender    = (*Unit)(nil)
	_ eventlog.SessionRows = (*Unit)(nil)
	_ crypto.Keys          = (*Unit)(nil)
)

// Unit returns the Unit for tx.
func (s *Sealer) Unit(tx pgx.Tx) *Unit {
	app := sealedAppender{s: s, tx: tx}
	return &Unit{Appender: app, app: app, tx: tx, keys: s.keys}
}

// InTenant runs fn in tenantID's scope with a Unit over the transaction, and
// commits if fn returns nil or rolls back if not.
func (s *Sealer) InTenant(ctx context.Context, st *store.Store, tenantID uuid.UUID, fn func(ctx context.Context, u *Unit) error) error {
	return st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, s.Unit(tx))
	})
}

// Tx is the underlying transaction, for a service's queries on its own tables.
func (u *Unit) Tx() pgx.Tx { return u.tx }

// GetSession implements eventlog.SessionRows.
func (u *Unit) GetSession(ctx context.Context, sessionID uuid.UUID) (eventlog.Session, error) {
	return store.GetSession(ctx, u.tx, sessionID)
}

// CreateSession implements eventlog.SessionRows.
func (u *Unit) CreateSession(ctx context.Context, sess eventlog.Session) error {
	return store.CreateSession(ctx, u.tx, sess)
}

// SetSessionStatus implements eventlog.SessionRows.
func (u *Unit) SetSessionStatus(ctx context.Context, sessionID uuid.UUID, status string, terminalReason *string) error {
	return store.UpdateSessionStatus(ctx, u.tx, sessionID, status, terminalReason)
}

// ListEvents implements eventlog.SessionRows.
func (u *Unit) ListEvents(ctx context.Context, sessionID uuid.UUID) ([]eventlog.Event, error) {
	return store.ListEvents(ctx, u.tx, sessionID)
}

// NewDEK implements crypto.Keys.
func (u *Unit) NewDEK(ctx context.Context, tenantID uuid.UUID) (crypto.DEK, error) {
	return u.keys.NewDEK(ctx, u.tx, tenantID)
}

// Unwrap implements crypto.Keys.
func (u *Unit) Unwrap(ctx context.Context, keyID string) (crypto.DEK, error) {
	return u.keys.Unwrap(ctx, u.tx, keyID)
}

// AppendFirst appends the session's very first event, sealed by seal rather than
// under a key found from earlier events (there are none to find). It is the one
// append that cannot use Append, whose active key comes from the session's own
// history; everything after it can.
func (u *Unit) AppendFirst(ctx context.Context, tenantID, sessionID uuid.UUID,
	seal func(plaintext []byte) (sealed, digest []byte, keyID string, err error),
	typ eventlog.EventType, payload any) (eventlog.Event, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return eventlog.Event{}, fmt.Errorf("eventlog: marshal %s payload: %w", typ, err)
	}
	sealedPayload, digest, keyID, err := seal(plaintext)
	if err != nil {
		return eventlog.Event{}, fmt.Errorf("eventlog: seal %s payload: %w", typ, err)
	}
	return u.app.appendSealed(ctx, tenantID, sessionID, eventlog.Event{
		EventID: uuid.New(), SessionID: sessionID, TenantID: tenantID, SchemaVersion: eventlog.CurrentSchemaVersion,
		Type: typ, Payload: sealedPayload, PayloadDigest: digest, KeyID: keyID, Actor: eventlog.ActorSystem,
	})
}
