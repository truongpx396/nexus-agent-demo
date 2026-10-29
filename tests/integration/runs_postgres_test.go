//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/controlplane"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
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
