//go:build integration

package integration

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditpg "github.com/truongpx396/nexus-agent/internal/audit/postgres"
	"github.com/truongpx396/nexus-agent/internal/eventlog"
	"github.com/truongpx396/nexus-agent/internal/eventlog/logtest"
	"github.com/truongpx396/nexus-agent/internal/eventlog/postgres"
	"github.com/truongpx396/nexus-agent/internal/store"
)

// failAfterSigner signs n digests and then fails, standing in for a signerd that
// goes away part-way through a batch.
type failAfterSigner struct {
	*fakeSigner
	remaining atomic.Int32
	err       error
}

func (s *failAfterSigner) Sign(ctx context.Context, digest []byte) ([]byte, string, error) {
	if s.remaining.Add(-1) < 0 {
		return nil, "", s.err
	}
	return s.fakeSigner.Sign(ctx, digest)
}

// A batch is one transaction with one receipt per event. If the signer goes away
// on the third, the first two events and their receipts roll back with it: a run
// never opens half written.
func TestPostgresEventLog_AppendBatchIsOneTransactionWithOneReceiptPerEvent(t *testing.T) {
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
	mk := func(typ eventlog.EventType) eventlog.Event {
		return eventlog.Event{
			EventID: uuid.New(), SessionID: sessionID, TenantID: tenantID, SchemaVersion: eventlog.CurrentSchemaVersion,
			Type: typ, Payload: []byte("p"), PayloadDigest: []byte("d"), KeyID: "k", Actor: eventlog.ActorSystem,
		}
	}
	batch := []eventlog.Event{mk(eventlog.EventToolLoaded), mk(eventlog.EventToolLoaded), mk(eventlog.EventToolLoaded)}
	counts := func() (events, receipts int) {
		t.Helper()
		if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1`, sessionID).Scan(&events); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM audit_receipts WHERE session_id = $1`, sessionID).Scan(&receipts)
		}); err != nil {
			t.Fatalf("count: %v", err)
		}
		return events, receipts
	}

	boom := errors.New("signer went away")
	signer := &failAfterSigner{fakeSigner: newFakeSigner(t), err: boom}
	signer.remaining.Store(2) // signs two receipts, fails the third
	if _, err := postgres.New(st, postgres.Hooks{Chain: auditpg.NewChain(signer)}).AppendBatch(ctx, batch); !errors.Is(err, boom) {
		t.Fatalf("AppendBatch with a signer that fails on the third = %v, want %v", err, boom)
	}
	if e, r := counts(); e != 0 || r != 0 {
		t.Fatalf("after the failed batch: %d events and %d receipts, want none of either", e, r)
	}

	signer.remaining.Store(100)
	got, err := postgres.New(st, postgres.Hooks{Chain: auditpg.NewChain(signer)}).AppendBatch(ctx, batch)
	if err != nil || len(got) != 3 || got[0].Seq != 1 || got[2].Seq != 3 {
		t.Fatalf("AppendBatch after the signer recovered = %v, %v, want three events numbered 1 to 3: a rolled-back batch must not consume sequence numbers", got, err)
	}
	if e, r := counts(); e != 3 || r != 3 {
		t.Fatalf("after the batch: %d events and %d receipts, want 3 and 3", e, r)
	}
}

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

// The status projection commits with the event that justifies it. If the receipt
// cannot be signed the event rolls back, and the status must go with it: a status
// that moved without its event is a session nothing in the log explains.
func TestPostgresEventLog_AppendAndSetStatusRollsBackTheStatusWithTheEvent(t *testing.T) {
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

	why := "completed"
	e := eventlog.Event{
		EventID: uuid.New(), SessionID: sessionID, TenantID: tenantID, SchemaVersion: eventlog.CurrentSchemaVersion,
		Type: eventlog.EventTerminal, Payload: []byte("p"), PayloadDigest: []byte("d"), KeyID: "k", Actor: eventlog.ActorSystem,
	}
	if _, err := log.AppendAndSetStatus(ctx, e, eventlog.SessionStatusCompleted, &why); !errors.Is(err, errReceipt) {
		t.Fatalf("AppendAndSetStatus with an unreachable signer = %v, want %v", err, errReceipt)
	}

	var sess eventlog.Session
	var events []eventlog.Event
	if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var gerr error
		if sess, gerr = store.GetSession(ctx, tx, sessionID); gerr != nil {
			return gerr
		}
		events, gerr = store.ListEvents(ctx, tx, sessionID)
		return gerr
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if sess.Status != eventlog.SessionStatusQueued || sess.TerminalReason != nil || len(events) != 0 {
		t.Fatalf("after the rollback: status %q reason %v with %d events, want the session untouched", sess.Status, sess.TerminalReason, len(events))
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
