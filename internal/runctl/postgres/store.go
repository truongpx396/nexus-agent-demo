// Package postgres is the Postgres adapter for internal/runctl: the claims
// ledger and the session writes cancel, steer, resume and fork make, each in
// the same transaction as the sealed event that records it. The rules — which
// session states allow which operation — are runctl's own.
package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	auditpg "github.com/truongpx396/nexus-agent-demo/internal/audit/postgres"
	cryptopg "github.com/truongpx396/nexus-agent-demo/internal/crypto/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/sealed"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// Store implements runctl.Store over Postgres. Every call runs in the named
// tenant's own scope (row-level security).
type Store struct {
	st     *store.Store
	sealer *sealed.Sealer
}

var _ runctl.Store = (*Store)(nil)

// New returns a Store. keys seals each event under the session's own key and
// mints keys for a fork; chain may be nil, in which case events are appended
// without a signed receipt (a minimal wiring for tests).
func New(st *store.Store, keys *cryptopg.KeyStore, chain *auditpg.Chain) *Store {
	return &Store{st: st, sealer: sealed.NewSealer(keys, chain)}
}

// InTenant implements runctl.Store.
func (s *Store) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx runctl.Tx) error) error {
	return s.sealer.InTenant(ctx, s.st, tenantID, func(ctx context.Context, u *sealed.Unit) error {
		return fn(ctx, &txn{Unit: u})
	})
}

// txn is the runctl.Tx over one pgx transaction.
type txn struct{ *sealed.Unit }

var _ runctl.Tx = (*txn)(nil)

func toClaim(c store.Claim) runctl.Claim {
	return runctl.Claim{
		ClaimID: c.ClaimID, TenantID: c.TenantID, SessionID: c.SessionID, ToolID: c.ToolID,
		CanonicalDigest: c.CanonicalDigest, Status: runctl.ClaimStatus(c.Status), Reason: c.Reason,
		CreatedAt: c.CreatedAt, ResolvedAt: c.ResolvedAt,
	}
}

func (t *txn) OpenOrFindClaim(ctx context.Context, tenantID, sessionID uuid.UUID, toolID string, digest []byte) (runctl.Claim, bool, error) {
	c, created, err := store.OpenOrFindClaim(ctx, t.Tx(), tenantID, sessionID, toolID, digest)
	if err != nil {
		return runctl.Claim{}, false, err
	}
	return toClaim(c), created, nil
}

func (t *txn) ResolveClaim(ctx context.Context, claimID uuid.UUID, status runctl.ClaimStatus, reason string) (runctl.Claim, error) {
	c, err := store.ResolveClaim(ctx, t.Tx(), claimID, store.ClaimStatus(status), reason)
	if err != nil {
		return runctl.Claim{}, err
	}
	return toClaim(c), nil
}

func (t *txn) ListInFlightClaims(ctx context.Context, sessionID uuid.UUID) ([]runctl.Claim, error) {
	rows, err := store.ListInFlightClaims(ctx, t.Tx(), sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]runctl.Claim, len(rows))
	for i, c := range rows {
		out[i] = toClaim(c)
	}
	return out, nil
}

func (t *txn) ClaimAwaitingInput(ctx context.Context, sessionID uuid.UUID) (bool, error) {
	return store.ClaimAwaitingInput(ctx, t.Tx(), sessionID)
}

func (t *txn) SetAutonomyLevel(ctx context.Context, sessionID uuid.UUID, level string) error {
	if _, err := t.Tx().Exec(ctx, `UPDATE sessions SET autonomy_level = $2 WHERE session_id = $1`, sessionID, level); err != nil {
		return fmt.Errorf("runctl: update autonomy_level: %w", err)
	}
	return nil
}
