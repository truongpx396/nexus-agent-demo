//go:build integration

package integration

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditpg "github.com/truongpx396/nexus-agent-demo/internal/audit/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/logtest"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// TestPostgresEventLog_SatisfiesTheEventLogContract runs the same suite the
// in-memory adapter passes (internal/eventlog/inmem) against the real thing,
// through PgBouncer's transaction-pooling tier as nexus_app — the path
// production uses (docs/constitution.md, Principle VI).
func TestPostgresEventLog_SatisfiesTheEventLogContract(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	t.Cleanup(cleanup)
	st := store.New(pool)
	ctx := context.Background()

	tenantID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	logtest.Run(t, logtest.Subject{
		Log: postgres.New(st, postgres.Hooks{}),
		NewSession: func(t *testing.T) (uuid.UUID, uuid.UUID) {
			t.Helper()
			sessionID := uuid.New()
			err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return store.CreateSession(ctx, tx, eventlog.Session{
					SessionID: sessionID, SessionKey: sessionID.String(), TenantID: tenantID,
					SurfaceID: "test", UserID: uuid.New(), AgentVersion: 1,
					HarnessDigest: []byte("test-digest"), DataLabel: "internal", RouteModelID: "fake",
					AutonomyLevel: "supervised",
				})
			})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			return tenantID, sessionID
		},
		Events: func(t *testing.T, tenantID, sessionID uuid.UUID) []eventlog.Event {
			t.Helper()
			var events []eventlog.Event
			err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var lerr error
				events, lerr = store.ListEvents(ctx, tx, sessionID)
				return lerr
			})
			if err != nil {
				t.Fatalf("list events: %v", err)
			}
			return events
		},
		Status: func(t *testing.T, tenantID, sessionID uuid.UUID) (string, *string) {
			t.Helper()
			var sess eventlog.Session
			err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var gerr error
				sess, gerr = store.GetSession(ctx, tx, sessionID)
				return gerr
			})
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			return sess.Status, sess.TerminalReason
		},
		SetAutonomy: func(t *testing.T, tenantID, sessionID uuid.UUID, level string) {
			t.Helper()
			err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, uerr := tx.Exec(ctx, `UPDATE sessions SET autonomy_level = $2 WHERE session_id = $1`, sessionID, level)
				return uerr
			})
			if err != nil {
				t.Fatalf("set autonomy: %v", err)
			}
		},
	})
}

// TestPostgresEventLog_ReceiptFailureRollsBackTheEvent pins the atomicity the
// port promises: an event whose audit receipt cannot be written is not
// observable (Principle VI — "an event is never observable without a receipt
// for it"). The hook runs inside the append's transaction, so its failure must
// leave no event behind and no sequence number consumed.
func TestPostgresEventLog_ReceiptFailureRollsBackTheEvent(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	t.Cleanup(cleanup)
	st := store.New(pool)
	ctx := context.Background()

	tenantID, sessionID := uuid.New(), uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.CreateSession(ctx, tx, eventlog.Session{
			SessionID: sessionID, SessionKey: sessionID.String(), TenantID: tenantID,
			SurfaceID: "test", UserID: uuid.New(), AgentVersion: 1,
			HarnessDigest: []byte("d"), DataLabel: "internal", RouteModelID: "fake", AutonomyLevel: "supervised",
		})
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	errReceipt := errors.New("signer unreachable")
	signer := &flakySigner{fakeSigner: newFakeSigner(t), err: errReceipt}
	signer.failing.Store(true)
	log := postgres.New(st, postgres.Hooks{Chain: auditpg.NewChain(signer)})

	e := eventlog.Event{
		EventID: uuid.New(), SessionID: sessionID, TenantID: tenantID, SchemaVersion: eventlog.CurrentSchemaVersion,
		Type: eventlog.EventUserMessage, Payload: []byte("p"), PayloadDigest: []byte("d"), KeyID: "k", Actor: eventlog.ActorUser,
	}
	if _, err := log.Append(ctx, e); !errors.Is(err, errReceipt) {
		t.Fatalf("Append with a failing receipt hook = %v, want %v", err, errReceipt)
	}

	signer.failing.Store(false)
	got, err := log.Append(ctx, e)
	if err != nil {
		t.Fatalf("Append after the receipt hook recovered: %v", err)
	}
	if got.Seq != 1 {
		t.Fatalf("Seq = %d after a rolled-back append, want 1 (the failed attempt must not consume a sequence number)", got.Seq)
	}
}

// flakySigner is a signer that can be made to fail, standing in for an
// unreachable signerd: the audit chain wraps its error, so the append that
// needed a receipt rolls back with it.
type flakySigner struct {
	*fakeSigner
	err     error
	failing atomic.Bool
}

func (s *flakySigner) Sign(ctx context.Context, digest []byte) ([]byte, string, error) {
	if s.failing.Load() {
		return nil, "", s.err
	}
	return s.fakeSigner.Sign(ctx, digest)
}
