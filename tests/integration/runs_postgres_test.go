//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/controlplane"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	eventlogpg "github.com/truongpx396/nexus-agent-demo/internal/eventlog/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/runs"
	runspg "github.com/truongpx396/nexus-agent-demo/internal/runs/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

func admitReq(tenantID uuid.UUID) controlplane.AdmitRunV1 {
	return controlplane.AdmitRunV1{
		TenantID: tenantID, UserID: uuid.New(), SessionID: uuid.New(), SurfaceID: "telegram",
		DataLabel: "internal", RouteModelID: "fake", Autonomy: "supervised", HarnessDigest: []byte("d"),
	}
}

// TestSessions_CreateSession pins what the control plane's durable half
// promises, through PgBouncer as nexus_app: the session key is the caller's
// (so a later message finds the session again) or the session id, and a
// requested ceiling becomes a budget row in the SAME transaction.
func TestSessions_CreateSession(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	t.Cleanup(cleanup)
	st := store.New(pool)
	ctx := context.Background()
	tenantID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	sessions := runspg.Sessions{Store: st}

	t.Run("a caller-supplied session key is stored, and the conversational flag with it", func(t *testing.T) {
		req := admitReq(tenantID)
		req.SessionKey, req.Conversational = "telegram:42", true
		res, err := sessions.CreateSession(ctx, req)
		if err != nil || !res.Admitted {
			t.Fatalf("CreateSession = %+v, %v", res, err)
		}
		sess, err := getSessionDirect(ctx, st, tenantID, req.SessionID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if sess.SessionKey != "telegram:42" || !sess.Conversational || sess.SurfaceID != "telegram" {
			t.Fatalf("session = key %q conversational %v surface %q", sess.SessionKey, sess.Conversational, sess.SurfaceID)
		}
	})

	t.Run("no session key means the session is addressed by its own id", func(t *testing.T) {
		req := admitReq(tenantID)
		if _, err := sessions.CreateSession(ctx, req); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		sess, _ := getSessionDirect(ctx, st, tenantID, req.SessionID)
		if sess.SessionKey != req.SessionID.String() {
			t.Fatalf("SessionKey = %q, want the session id %s", sess.SessionKey, req.SessionID)
		}
	})

	t.Run("a budget ceiling is created with the session", func(t *testing.T) {
		req := admitReq(tenantID)
		req.BudgetUSD = "0.05"
		if res, err := sessions.CreateSession(ctx, req); err != nil || !res.Admitted {
			t.Fatalf("CreateSession = %+v, %v", res, err)
		}
		var n int
		err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM budgets WHERE scope_ref = $1`, req.SessionID).Scan(&n)
		})
		if err != nil || n != 1 {
			t.Fatalf("session-scoped budget rows = %d, %v; want exactly 1", n, err)
		}
	})

	t.Run("a malformed ceiling is a refusal, not an error, and creates nothing", func(t *testing.T) {
		req := admitReq(tenantID)
		req.BudgetUSD = "five dollars"
		res, err := sessions.CreateSession(ctx, req)
		if err != nil {
			t.Fatalf("CreateSession returned an error for a caller mistake: %v", err)
		}
		if res.Admitted || res.Reason == "" {
			t.Fatalf("result = %+v, want Admitted=false with a Reason", res)
		}
		if _, err := getSessionDirect(ctx, st, tenantID, req.SessionID); err == nil {
			t.Fatal("a refused request left a session row behind")
		}
	})
}

// TestKeys_NewDEK pins that the provider mints a key the data plane can seal
// with now and unwrap again later (a surface decrypts events with the
// persisted, wrapped copy, not the in-memory one).
func TestKeys_NewDEK(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	t.Cleanup(cleanup)
	st := store.New(pool)
	ctx := context.Background()
	tenantID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	ks := crypto.NewKeyStore(kek)

	dek, err := runspg.Keys{Store: st, KeyStore: ks}.NewDEK(ctx, tenantID)
	if err != nil {
		t.Fatalf("NewDEK: %v", err)
	}

	var unwrapped crypto.DEK
	if err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var uerr error
		unwrapped, uerr = ks.Unwrap(ctx, tx, dek.KeyID)
		return uerr
	}); err != nil {
		t.Fatalf("the minted key was not persisted: %v", err)
	}
	sealed, err := crypto.Seal(dek, []byte("secret"), tenantID.String(), "s")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got, err := crypto.Open(unwrapped, sealed, tenantID.String(), "s"); err != nil || string(got) != "secret" {
		t.Fatalf("Open with the persisted key = %q, %v; want the plaintext", got, err)
	}
}

// TestReader pins the read side through PgBouncer as nexus_app, above all that
// another tenant's run is indistinguishable from one that does not exist.
func TestReader(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	t.Cleanup(cleanup)
	st := store.New(pool)
	ctx := context.Background()

	tenantA, tenantB, user := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{tenantA, tenantB} {
		if err := insertTenant(ctx, st, id); err != nil {
			t.Fatalf("insert tenant: %v", err)
		}
	}
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	ks := crypto.NewKeyStore(kek)
	reader := runspg.Reader{Store: st, KeyStore: ks}
	log := eventlogpg.New(st, eventlogpg.Hooks{})

	admit := func(tenantID, userID uuid.UUID) uuid.UUID {
		req := admitReq(tenantID)
		req.UserID = userID
		if res, err := (runspg.Sessions{Store: st}).CreateSession(ctx, req); err != nil || !res.Admitted {
			t.Fatalf("CreateSession = %+v, %v", res, err)
		}
		return req.SessionID
	}
	older, newer := admit(tenantA, user), admit(tenantA, user)
	_ = admit(tenantA, uuid.New()) // another user's run in the same tenant
	otherTenantRun := admit(tenantB, user)

	t.Run("GetRun returns the row, and ErrNotFound for unknown or foreign-tenant ids alike", func(t *testing.T) {
		run, err := reader.GetRun(ctx, tenantA, newer)
		if err != nil || run.RunID != newer || run.UserID != user || run.Status != eventlog.SessionStatusQueued {
			t.Fatalf("GetRun = %+v, %v", run, err)
		}
		if _, err := reader.GetRun(ctx, tenantA, uuid.New()); !errors.Is(err, runs.ErrNotFound) {
			t.Errorf("unknown id: err = %v, want runs.ErrNotFound", err)
		}
		if _, err := reader.GetRun(ctx, tenantA, otherTenantRun); !errors.Is(err, runs.ErrNotFound) {
			t.Errorf("another tenant's run: err = %v, want runs.ErrNotFound (RLS must make it indistinguishable from absent)", err)
		}
	})

	t.Run("ListRuns is the user's own, newest first, and honours the limit", func(t *testing.T) {
		all, err := reader.ListRuns(ctx, tenantA, user, 10)
		if err != nil || len(all) != 2 || all[0].RunID != newer || all[1].RunID != older {
			t.Fatalf("ListRuns = %+v, %v; want [newer, older] and no other user's run", all, err)
		}
		if one, _ := reader.ListRuns(ctx, tenantA, user, 1); len(one) != 1 {
			t.Errorf("limit 1 returned %d runs", len(one))
		}
		if foreign, _ := reader.ListRuns(ctx, tenantB, user, 10); len(foreign) != 1 || foreign[0].RunID != otherTenantRun {
			t.Errorf("tenant B sees %+v, want only its own run", foreign)
		}
	})

	t.Run("FindByKey returns the most recent run under a key, and nothing for an unknown key or another tenant", func(t *testing.T) {
		const key = "telegram:chat-42"
		admitKeyed := func(tenantID uuid.UUID) uuid.UUID {
			req := admitReq(tenantID)
			req.UserID = user
			req.SessionKey = key
			if res, err := (runspg.Sessions{Store: st}).CreateSession(ctx, req); err != nil || !res.Admitted {
				t.Fatalf("CreateSession = %+v, %v", res, err)
			}
			return req.SessionID
		}
		_, second := admitKeyed(tenantA), admitKeyed(tenantA)

		run, found, err := reader.FindByKey(ctx, tenantA, key)
		if err != nil || !found || run.RunID != second || run.Status != eventlog.SessionStatusQueued {
			t.Fatalf("FindByKey = (%+v, %v, %v), want the newer of the two runs sharing the key", run, found, err)
		}
		if _, found, err := reader.FindByKey(ctx, tenantA, "telegram:nobody"); err != nil || found {
			t.Errorf("an unknown key = (found=%v, err=%v), want (false, nil): no run yet is an answer, not an error", found, err)
		}
		if _, found, _ := reader.FindByKey(ctx, tenantB, key); found {
			t.Error("another tenant found this tenant's run by its key: RLS must make it indistinguishable from absent")
		}
	})

	t.Run("Events come back in order and open to the plaintext, only for their own tenant", func(t *testing.T) {
		dek, err := runspg.Keys{Store: st, KeyStore: ks}.NewDEK(ctx, tenantA)
		if err != nil {
			t.Fatalf("NewDEK: %v", err)
		}
		seal := runs.SealFor(dek, tenantA, newer)
		for _, body := range []string{"first", "second"} {
			sealed, digest, keyID, err := seal([]byte(`{"body":"` + body + `"}`))
			if err != nil {
				t.Fatalf("seal: %v", err)
			}
			if _, err := log.Append(ctx, eventlog.Event{
				EventID: uuid.New(), SessionID: newer, TenantID: tenantA, SchemaVersion: eventlog.CurrentSchemaVersion,
				Type: eventlog.EventContent, Payload: sealed, PayloadDigest: digest, KeyID: keyID, Actor: eventlog.ActorModel,
			}); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}

		events, err := reader.Events(ctx, tenantA, newer)
		if err != nil || len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 {
			t.Fatalf("Events = %d event(s), %v; want 2 in sequence order", len(events), err)
		}
		opener := reader.EventOpener(tenantA)
		for i, want := range []string{`{"body":"first"}`, `{"body":"second"}`} {
			if got, err := opener.Open(ctx, events[i]); err != nil || string(got) != want {
				t.Errorf("Open(event %d) = %q, %v; want %q", i, got, err, want)
			}
		}
		if _, err := reader.EventOpener(tenantB).Open(ctx, events[0]); err == nil {
			t.Error("tenant B opened tenant A's event")
		}
	})
}
