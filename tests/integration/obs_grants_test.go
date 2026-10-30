//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/obs"
	obspg "github.com/truongpx396/nexus-agent-demo/internal/obs/postgres"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
)

// grantsRig is a session whose log holds a few really-encrypted events, behind
// the real Postgres GrantStore and a real audit chain.
type grantsRig struct {
	*oversightRig
	grants    *obs.Grants
	store     *obspg.GrantStore
	sessionID uuid.UUID
}

func setupGrantsRig(t *testing.T) *grantsRig {
	t.Helper()
	r := setupOversightRig(t)
	ctx := context.Background()
	sessionID := uuid.New()
	r.createSession(t, sessionID, uuid.New(), "autonomous")
	seal := r.seal(t, sessionID)

	err := r.st.InTenantTx(ctx, r.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, e := range []struct {
			typ  eventlog.EventType
			body string
		}{
			{eventlog.EventUserMessage, `{"text":"the account number is 4411"}`},
			{eventlog.EventThought, `{"text":"private reasoning"}`},
			{eventlog.EventUserMessage, `{"text":"thanks"}`},
		} {
			sealed, digest, keyID, err := seal([]byte(e.body))
			if err != nil {
				return err
			}
			if _, err := store.Append(ctx, tx, eventlog.Event{
				EventID: uuid.New(), SessionID: sessionID, TenantID: r.tenantID,
				SchemaVersion: eventlog.CurrentSchemaVersion, Type: e.typ,
				Payload: sealed, PayloadDigest: digest, KeyID: keyID, Actor: eventlog.ActorUser,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed events: %v", err)
	}

	gs := obspg.NewGrantStore(r.st, r.keys, r.chain)
	return &grantsRig{oversightRig: r, grants: obs.NewGrants(gs), store: gs, sessionID: sessionID}
}

func (g *grantsRig) eventTypes(t *testing.T) []eventlog.EventType {
	t.Helper()
	events, err := listEventsDirect(context.Background(), g.st, g.tenantID, g.sessionID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	out := make([]eventlog.EventType, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func (g *grantsRig) count(t *testing.T, typ eventlog.EventType) int {
	t.Helper()
	n := 0
	for _, got := range g.eventTypes(t) {
		if got == typ {
			n++
		}
	}
	return n
}

func (g *grantsRig) receipts(t *testing.T) int {
	t.Helper()
	var n int
	err := g.st.InTenantTx(context.Background(), g.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_receipts WHERE session_id = $1`, g.sessionID).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count receipts: %v", err)
	}
	return n
}

func TestGrants_APostgresBackedGrantIsAuditedAndUnlocksTheHistoryOnce(t *testing.T) {
	g := setupGrantsRig(t)
	ctx := context.Background()
	grantee := uuid.New()

	before := g.receipts(t)
	gr, err := g.grants.RequestGrant(ctx, g.tenantID, g.sessionID, grantee, "support ticket 4411", time.Hour)
	if err != nil {
		t.Fatalf("RequestGrant: %v", err)
	}
	if gr.CreatedAt.IsZero() {
		t.Error("the stored grant has no created_at: the adapter must return the row it wrote")
	}
	if g.count(t, eventlog.EventContentAccessGranted) != 1 {
		t.Error("issuing a grant appended no content_access_granted event")
	}
	if g.receipts(t) != before+1 {
		t.Errorf("audit receipts went %d -> %d, want one more for the grant", before, g.receipts(t))
	}

	history, err := g.grants.Read(ctx, g.tenantID, g.sessionID, grantee)
	if err != nil {
		t.Fatalf("Read under a valid grant: %v", err)
	}
	var sawAccount, sawThoughtBody bool
	for _, e := range history {
		var body struct{ Text string }
		_ = json.Unmarshal(e.Body, &body)
		if body.Text == "the account number is 4411" {
			sawAccount = true
		}
		if e.Type == eventlog.EventThought && len(e.Body) != 0 {
			sawThoughtBody = true
		}
	}
	if !sawAccount {
		t.Error("the granted read did not return the decrypted message")
	}
	if sawThoughtBody {
		t.Error("a thought's body was returned under a grant: reasoning is never shown")
	}
	if got := g.count(t, eventlog.EventContentAccessed); got != 1 {
		t.Errorf("%d content_accessed receipts after one read, want exactly 1", got)
	}
}

func TestGrants_APostgresBackedRefusalIsAudited(t *testing.T) {
	g := setupGrantsRig(t)
	ctx := context.Background()

	history, err := g.grants.Read(ctx, g.tenantID, g.sessionID, uuid.New())
	if err == nil || history != nil {
		t.Fatalf("Read with no grant = (%v, %v), want a refusal and no content", history, err)
	}
	if g.count(t, eventlog.EventContentAccessRefused) != 1 {
		t.Error("a denied attempt appended no content_access_refused event")
	}
	if g.count(t, eventlog.EventContentAccessed) != 0 {
		t.Error("a denied attempt recorded an access receipt")
	}
}

func TestGrants_ARevokedOrExpiredGrantNoLongerUnlocksTheHistory(t *testing.T) {
	g := setupGrantsRig(t)
	ctx := context.Background()
	grantee := uuid.New()

	revoked, err := g.grants.RequestGrant(ctx, g.tenantID, g.sessionID, grantee, "r", time.Hour)
	if err != nil {
		t.Fatalf("RequestGrant: %v", err)
	}
	if _, err := g.grants.Read(ctx, g.tenantID, g.sessionID, grantee); err != nil {
		t.Fatalf("Read under a fresh grant: %v", err)
	}

	if err := g.st.InTenantTx(ctx, g.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE content_access_grants SET revoked_at = now() WHERE grant_id = $1`, revoked.GrantID)
		return err
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := g.grants.Read(ctx, g.tenantID, g.sessionID, grantee); err == nil {
		t.Error("Read succeeded under a revoked grant")
	}

	other := uuid.New()
	if _, err := g.grants.RequestGrant(ctx, g.tenantID, g.sessionID, other, "r", time.Hour); err != nil {
		t.Fatalf("RequestGrant: %v", err)
	}
	future := &obs.Grants{Store: g.store, Now: func() time.Time { return time.Now().Add(2 * time.Hour) }}
	if _, err := future.Read(ctx, g.tenantID, g.sessionID, other); err == nil {
		t.Error("Read succeeded two hours after a one-hour grant")
	}
}

func TestGrantStore_OneTenantCannotSeeAnothersGrants(t *testing.T) {
	g := setupGrantsRig(t)
	ctx := context.Background()
	grantee := uuid.New()
	if _, err := g.grants.RequestGrant(ctx, g.tenantID, g.sessionID, grantee, "r", time.Hour); err != nil {
		t.Fatalf("RequestGrant: %v", err)
	}

	otherTenant := uuid.New()
	if err := insertTenant(ctx, g.st, otherTenant); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	got, err := g.store.ListFor(ctx, otherTenant, g.sessionID, grantee)
	if err != nil {
		t.Fatalf("ListFor: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("another tenant listed %d of this tenant's grants: RLS, not the query, must keep them apart", len(got))
	}
}
