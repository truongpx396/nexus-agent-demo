//go:build integration

package zalo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// setupZaloEnv mirrors internal/surfaces/telegram's own setupTelegramEnv
// (this codebase's established per-file duplication idiom for integration
// test scaffolding).
func setupZaloEnv(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	pgReq := testcontainers.ContainerRequest{
		Image:        "pgvector/pgvector:pg17", // needs CREATE EXTENSION vector (migrations/0022_retrieval.sql)
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "nexus",
			"POSTGRES_PASSWORD": "nexus",
			"POSTGRES_DB":       "nexus",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(120 * time.Second),
	}
	pgC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: pgReq, Started: true})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })

	host, err := pgC.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := pgC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}

	migrateDSN := fmt.Sprintf("postgres://nexus:nexus@%s:%s/nexus", host, port.Port())
	migratePool, err := pgxpool.New(ctx, migrateDSN)
	if err != nil {
		t.Fatalf("connect as migration role: %v", err)
	}
	defer migratePool.Close()
	if _, err := store.Migrate(ctx, migratePool, migrations.FS); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	appDSN := fmt.Sprintf("postgres://nexus_app:nexus_app@%s:%s/nexus", host, port.Port())
	appPool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect as nexus_app: %v", err)
	}
	t.Cleanup(appPool.Close)
	return appPool
}

func insertTestTenant(t *testing.T, s *store.Store, tenantID uuid.UUID) {
	t.Helper()
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'zalo-test')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
}

func newZaloWebhookBody(t *testing.T, senderID, text string) []byte {
	t.Helper()
	// No msg_id — matches every pre-delivery-dedup test fixture in this
	// file: store.ClaimInboundDelivery's own documented empty-string
	// behavior (always claims, never dedupes) means these keep passing
	// unmodified. Only a test that specifically exercises dedup needs a
	// real, distinct msg_id (newZaloWebhookBodyWithMsgID).
	return newZaloWebhookBodyWithMsgID(t, "", senderID, text)
}

func newZaloWebhookBodyWithMsgID(t *testing.T, msgID, senderID, text string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"app_id":     "app-1",
		"event_name": "user_send_text",
		"sender":     map[string]string{"id": senderID},
		"message":    map[string]string{"text": text, "msg_id": msgID},
	})
	if err != nil {
		t.Fatalf("marshal webhook body: %v", err)
	}
	return body
}

func postZaloWebhook(t *testing.T, srv *Server, tenantID uuid.UUID, secret string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/zalo/"+tenantID.String(), bytes.NewReader(body))
	req.Header.Set("X-ZEvent-Signature", sign(body, secret))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestHandleWebhook_CreatesConversationalSessionWithDeterministicKey mirrors
// internal/surfaces/telegram's own (its doc comment).
func TestHandleWebhook_CreatesConversationalSessionWithDeterministicKey(t *testing.T) {
	pool := setupZaloEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	insertTestTenant(t, s, tenantID)
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	starter := &fakeStarter{}
	srv := &Server{Store: s, KeyStore: crypto.NewKeyStore(kek), Starter: starter, Channels: fakeChannels{secret: "s", ok: true}}

	rec := postZaloWebhook(t, srv, tenantID, "s", newZaloWebhookBody(t, "user-1", "please help"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}

	var sessionKey string
	var conversational bool
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT session_key, conversational FROM sessions WHERE session_id = $1`, starter.req.SessionID).Scan(&sessionKey, &conversational)
	}); err != nil {
		t.Fatalf("load created session: %v", err)
	}
	if sessionKey != "zalo:user-1" {
		t.Fatalf("session_key = %q, want %q", sessionKey, "zalo:user-1")
	}
	if !conversational {
		t.Fatal("conversational = false, want true")
	}
}

// TestHandleWebhook_SecondMessageFromSamePeerResumesSameSession mirrors
// internal/surfaces/telegram's own (its doc comment).
func TestHandleWebhook_SecondMessageFromSamePeerResumesSameSession(t *testing.T) {
	pool := setupZaloEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	insertTestTenant(t, s, tenantID)
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	starter := &fakeStarter{}
	resumer := &fakeResumer{}
	srv := &Server{
		Store: s, KeyStore: crypto.NewKeyStore(kek), Starter: starter, Resume: resumer,
		Channels: fakeChannels{secret: "s", ok: true},
	}

	rec := postZaloWebhook(t, srv, tenantID, "s", newZaloWebhookBody(t, "user-1", "first message"))
	if rec.Code != http.StatusOK {
		t.Fatalf("first message: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	firstSessionID := starter.req.SessionID
	if resumer.resumed {
		t.Fatal("the FIRST message for a new peer must not resume anything")
	}

	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET status = $2 WHERE session_id = $1`, firstSessionID, store.SessionStatusAwaitingInput)
		return err
	}); err != nil {
		t.Fatalf("mark session awaiting_input: %v", err)
	}

	rec = postZaloWebhook(t, srv, tenantID, "s", newZaloWebhookBody(t, "user-1", "second message"))
	if rec.Code != http.StatusOK {
		t.Fatalf("second message: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !resumer.resumed {
		t.Fatal("the second message from the SAME sender did not resume")
	}
	if resumer.sessionID != firstSessionID {
		t.Fatalf("resumed session id = %s, want the first message's own session %s", resumer.sessionID, firstSessionID)
	}
	if starter.req.SessionID != firstSessionID {
		t.Fatal("StartRun was called a second time — the resumed message must never also create a fresh session")
	}
}

// TestHandleWebhook_SamePeerAfterNonAwaitingInputStartsFreshReusingKey
// mirrors internal/surfaces/telegram's own (its doc comment).
func TestHandleWebhook_SamePeerAfterNonAwaitingInputStartsFreshReusingKey(t *testing.T) {
	pool := setupZaloEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	insertTestTenant(t, s, tenantID)
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	starter := &fakeStarter{}
	resumer := &fakeResumer{}
	srv := &Server{
		Store: s, KeyStore: crypto.NewKeyStore(kek), Starter: starter, Resume: resumer,
		Channels: fakeChannels{secret: "s", ok: true},
	}

	rec := postZaloWebhook(t, srv, tenantID, "s", newZaloWebhookBody(t, "user-1", "first message"))
	if rec.Code != http.StatusOK {
		t.Fatalf("first message: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	firstSessionID := starter.req.SessionID

	rec = postZaloWebhook(t, srv, tenantID, "s", newZaloWebhookBody(t, "user-1", "second message"))
	if rec.Code != http.StatusOK {
		t.Fatalf("second message: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if resumer.resumed {
		t.Fatal("a session that is not awaiting_input must never be resumed")
	}
	if starter.req.SessionID == firstSessionID {
		t.Fatal("expected a genuinely NEW session for the second message")
	}
	secondSessionID := starter.req.SessionID

	var firstKey, secondKey string
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT session_key FROM sessions WHERE session_id = $1`, firstSessionID).Scan(&firstKey); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT session_key FROM sessions WHERE session_id = $1`, secondSessionID).Scan(&secondKey)
	}); err != nil {
		t.Fatalf("load session keys: %v", err)
	}
	if firstKey != secondKey {
		t.Fatalf("session_key differs across the two sessions (%q vs %q), want the same deterministic key reused", firstKey, secondKey)
	}
}

// TestServer_NotificationPayload_DecryptsContentEvent mirrors
// internal/surfaces/telegram's own (its doc comment).
func TestServer_NotificationPayload_DecryptsContentEvent(t *testing.T) {
	pool := setupZaloEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	sessionID := uuid.New()
	insertTestTenant(t, s, tenantID)

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	keys := crypto.NewKeyStore(kek)
	var dek crypto.DEK
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		dek, derr = keys.NewDEK(ctx, tx, tenantID)
		return derr
	}); err != nil {
		t.Fatalf("NewDEK: %v", err)
	}

	plaintext, err := json.Marshal(map[string]string{"body": "here is my reply"})
	if err != nil {
		t.Fatalf("marshal plaintext: %v", err)
	}
	sealed, err := crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	srv := &Server{Store: s, KeyStore: keys}
	ev := store.Event{Type: store.EventContent, Payload: sealed, KeyID: dek.KeyID}
	payload, ok := srv.notificationPayload(context.Background(), tenantID, sessionID, ev)
	if !ok {
		t.Fatal("notificationPayload returned ok=false for a real content event")
	}
	var got notificationPayload
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal built payload: %v", err)
	}
	if got.Kind != "content" || got.Text != "here is my reply" {
		t.Fatalf("payload = %+v, want kind=content text=%q", got, "here is my reply")
	}
}

// TestHandleWebhook_DuplicateMsgIDAcknowledgedWithoutASecondRun is
// migrations/0024_inbound_deliveries.sql's own reason to exist
// (production-readiness review: "Telegram/Zalo webhooks retry by design"
// with nothing deduping that): the SAME msg_id delivered twice — Zalo
// itself redelivering after a slow/lost ack is the realistic trigger, not a
// client bug — must reach StartRun exactly once.
func TestHandleWebhook_DuplicateMsgIDAcknowledgedWithoutASecondRun(t *testing.T) {
	pool := setupZaloEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	insertTestTenant(t, s, tenantID)
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	starter := &fakeStarter{}
	srv := &Server{Store: s, KeyStore: crypto.NewKeyStore(kek), Starter: starter, Channels: fakeChannels{secret: "s", ok: true}}

	body := newZaloWebhookBodyWithMsgID(t, "msg-4242", "user-999", "please help")
	rec := postZaloWebhook(t, srv, tenantID, "s", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("first delivery: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if starter.calls != 1 {
		t.Fatalf("calls after first delivery = %d, want 1", starter.calls)
	}
	firstSessionID := starter.req.SessionID

	// The exact same msg_id, redelivered — the realistic Zalo-retry shape,
	// not a different sender/text.
	rec = postZaloWebhook(t, srv, tenantID, "s", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("redelivery: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if starter.calls != 1 {
		t.Fatalf("calls after redelivery = %d, want still 1 (StartRun must not run twice for one msg_id)", starter.calls)
	}
	if starter.req.SessionID != firstSessionID {
		t.Fatal("a redelivered msg_id must never reach StartRun with a different session")
	}

	var sessionCount int
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE session_key = $1`, "zalo:user-999").Scan(&sessionCount)
	}); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Fatalf("sessions for zalo:user-999 = %d, want exactly 1", sessionCount)
	}
}

// TestHandleWebhook_SessionLockAcquiredAndReleasedAroundDispatch proves
// s.Lock's own plumbing end to end: Acquire is called with the dispatched
// sessionKey, and Release is called with the SAME token only after the
// run's own event channel (drainAndNotify) has finished draining — closing
// the production-readiness review's other finding, that the REST/webhook
// direct-call path never touched SessionLock at all.
func TestHandleWebhook_SessionLockAcquiredAndReleasedAroundDispatch(t *testing.T) {
	pool := setupZaloEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	insertTestTenant(t, s, tenantID)
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	starter := &fakeStarter{}
	locker := &fakeLocker{acquireResults: []bool{true}, done: make(chan struct{})}
	srv := &Server{Store: s, KeyStore: crypto.NewKeyStore(kek), Starter: starter, Channels: fakeChannels{secret: "s", ok: true}, Lock: locker}

	rec := postZaloWebhook(t, srv, tenantID, "s", newZaloWebhookBody(t, "user-999", "please help"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}

	select {
	case <-locker.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Release — drainAndNotify never ran or never released the lock")
	}

	locker.mu.Lock()
	defer locker.mu.Unlock()
	if len(locker.acquireCalls) != 1 || locker.acquireCalls[0] != "zalo:user-999" {
		t.Fatalf("Acquire calls = %v, want exactly one for %q", locker.acquireCalls, "zalo:user-999")
	}
	if len(locker.releaseCalls) != 1 {
		t.Fatalf("Release calls = %v, want exactly one", locker.releaseCalls)
	}
	if locker.releaseCalls[0].sessionKey != "zalo:user-999" || locker.releaseCalls[0].token != "token-zalo:user-999" {
		t.Fatalf("Release call = %+v, want the same session key and token Acquire returned", locker.releaseCalls[0])
	}
}

// TestHandleWebhook_ContendedSessionLockDropsDeliveryWithoutStartingARun is
// dispatch's own documented trade-off under lock contention: if s.Lock
// never grants the lock (another goroutine is already driving this exact
// session_key's turn), this delivery is acknowledged but StartRun is never
// called — never a second concurrent turn for the same session, the whole
// point of wiring SessionLock into this path at all.
func TestHandleWebhook_ContendedSessionLockDropsDeliveryWithoutStartingARun(t *testing.T) {
	pool := setupZaloEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	insertTestTenant(t, s, tenantID)
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	starter := &fakeStarter{}
	// Every attempt reports contention — AcquireSessionLock's own bounded
	// retry (internal/surfaces/lock.go) exhausts all of them and gives up.
	locker := &fakeLocker{acquireResults: []bool{false, false, false, false, false}}
	srv := &Server{Store: s, KeyStore: crypto.NewKeyStore(kek), Starter: starter, Channels: fakeChannels{secret: "s", ok: true}, Lock: locker}

	rec := postZaloWebhook(t, srv, tenantID, "s", newZaloWebhookBody(t, "user-999", "please help"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 (an ack, even though nothing was dispatched)", rec.Code, rec.Body.String())
	}
	if starter.started {
		t.Fatal("StartRun ran despite the session lock never being granted")
	}
}
