package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Reader is the runs.Reader over Postgres. Every call runs in its own
// tenant-scoped transaction, so a run belonging to another tenant is
// indistinguishable from one that does not exist.
type Reader struct {
	Store    *store.Store
	KeyStore *crypto.KeyStore
}

var _ runs.Reader = Reader{}

func view(s store.Session) runs.RunView {
	return runs.RunView{
		RunID: s.SessionID, UserID: s.UserID, Status: s.Status, TerminalReason: s.TerminalReason,
		CreatedAt: s.CreatedAt, AutonomyLevel: s.AutonomyLevel,
	}
}

func (r Reader) GetRun(ctx context.Context, tenantID, runID uuid.UUID) (runs.RunView, error) {
	var sess store.Session
	err := r.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var gerr error
		sess, gerr = store.GetSession(ctx, tx, runID)
		return gerr
	})
	if errors.Is(err, store.ErrSessionNotFound) {
		return runs.RunView{}, runs.ErrNotFound
	}
	if err != nil {
		return runs.RunView{}, err
	}
	return view(sess), nil
}

func (r Reader) FindByKey(ctx context.Context, tenantID uuid.UUID, sessionKey string) (runs.RunView, bool, error) {
	var (
		sess  store.Session
		found bool
	)
	err := r.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var gerr error
		sess, found, gerr = store.GetSessionByKey(ctx, tx, tenantID, sessionKey)
		return gerr
	})
	if err != nil || !found {
		return runs.RunView{}, false, err
	}
	return view(sess), true, nil
}

func (r Reader) ListRuns(ctx context.Context, tenantID, userID uuid.UUID, limit int) ([]runs.RunView, error) {
	var sessions []store.Session
	err := r.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var lerr error
		sessions, lerr = store.ListSessionsForUser(ctx, tx, userID, limit)
		return lerr
	})
	if err != nil {
		return nil, err
	}
	views := make([]runs.RunView, len(sessions))
	for i, s := range sessions {
		views[i] = view(s)
	}
	return views, nil
}

func (r Reader) Events(ctx context.Context, tenantID, runID uuid.UUID) ([]eventlog.Event, error) {
	var events []eventlog.Event
	err := r.Store.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var lerr error
		events, lerr = store.ListEvents(ctx, tx, runID)
		return lerr
	})
	return events, err
}

func (r Reader) EventOpener(tenantID uuid.UUID) runs.EventOpener {
	return &opener{r: r, tenantID: tenantID, deks: map[string]crypto.DEK{}}
}

// opener unwraps each distinct key once per request (in practice every event
// in a session shares one) and keeps the plaintext key only for that
// request's lifetime.
type opener struct {
	r        Reader
	tenantID uuid.UUID
	deks     map[string]crypto.DEK
}

func (o *opener) Open(ctx context.Context, e eventlog.Event) ([]byte, error) {
	dek, ok := o.deks[e.KeyID]
	if !ok {
		err := o.r.Store.InTenantTx(ctx, o.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var uerr error
			dek, uerr = o.r.KeyStore.Unwrap(ctx, tx, e.KeyID)
			return uerr
		})
		if err != nil {
			return nil, fmt.Errorf("unwrap key for event %s: %w", e.EventID, err)
		}
		o.deks[e.KeyID] = dek
	}
	plaintext, err := crypto.Open(dek, e.Payload, o.tenantID.String(), e.SessionID.String())
	if err != nil {
		return nil, fmt.Errorf("decrypt event %s: %w", e.EventID, err)
	}
	return plaintext, nil
}
