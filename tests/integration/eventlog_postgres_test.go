//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
				return store.CreateSession(ctx, tx, store.Session{
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
			var sess store.Session
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
		return store.CreateSession(ctx, tx, store.Session{
			SessionID: sessionID, SessionKey: sessionID.String(), TenantID: tenantID,
			SurfaceID: "test", UserID: uuid.New(), AgentVersion: 1,
			HarnessDigest: []byte("d"), DataLabel: "internal", RouteModelID: "fake", AutonomyLevel: "supervised",
		})
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	errReceipt := errors.New("signer unreachable")
	failing := true
	log := postgres.New(st, postgres.Hooks{
		Receipt: func(context.Context, pgx.Tx, eventlog.Event) error {
			if failing {
				return errReceipt
			}
			return nil
		},
	})

	e := eventlog.Event{
		EventID: uuid.New(), SessionID: sessionID, TenantID: tenantID, SchemaVersion: eventlog.CurrentSchemaVersion,
		Type: eventlog.EventUserMessage, Payload: []byte("p"), PayloadDigest: []byte("d"), KeyID: "k", Actor: eventlog.ActorUser,
	}
	if _, err := log.Append(ctx, e); !errors.Is(err, errReceipt) {
		t.Fatalf("Append with a failing receipt hook = %v, want %v", err, errReceipt)
	}

	failing = false
	got, err := log.Append(ctx, e)
	if err != nil {
		t.Fatalf("Append after the receipt hook recovered: %v", err)
	}
	if got.Seq != 1 {
		t.Fatalf("Seq = %d after a rolled-back append, want 1 (the failed attempt must not consume a sequence number)", got.Seq)
	}
}
