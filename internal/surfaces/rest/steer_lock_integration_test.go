//go:build integration

package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/harness"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// setupSteerLockEnv mirrors internal/surfaces/telegram/webhook_integration_test.go's
// own setupTelegramEnv (this codebase's established per-file duplication
// idiom for integration test scaffolding not exercising PgBouncer itself).
func setupSteerLockEnv(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	pgReq := testcontainers.ContainerRequest{
		Image:        "pgvector/pgvector:pg17",
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

// fakeRunCtl is RunCtlPort's own minimal test double for this file — only
// ResumeConversation is exercised here.
type fakeRunCtl struct {
	resumed bool
}

func (f *fakeRunCtl) Cancel(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }
func (f *fakeRunCtl) Steer(context.Context, uuid.UUID, uuid.UUID, string) error  { return nil }
func (f *fakeRunCtl) TightenAutonomy(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}
func (f *fakeRunCtl) Fork(context.Context, uuid.UUID, uuid.UUID, int64, string) (ForkView, error) {
	return ForkView{}, nil
}
func (f *fakeRunCtl) ResumeConversation(_ context.Context, _, _ uuid.UUID, _ string) (<-chan RunEvent, error) {
	f.resumed = true
	ch := make(chan RunEvent)
	close(ch)
	return ch, nil
}

// fakeSteerLocker mirrors internal/surfaces/telegram's own fakeLocker (this
// codebase's established per-package test-double duplication idiom).
type fakeSteerLocker struct {
	mu             sync.Mutex
	acquireResults []bool
	acquireCalls   []string
	releaseCalls   []string
	done           chan struct{}
}

func (f *fakeSteerLocker) Acquire(_ context.Context, sessionKey string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquireCalls = append(f.acquireCalls, sessionKey)
	ok := false
	if len(f.acquireResults) > 0 {
		ok = f.acquireResults[0]
		f.acquireResults = f.acquireResults[1:]
	}
	if !ok {
		return "", false, nil
	}
	return "token-" + sessionKey, true, nil
}

func (f *fakeSteerLocker) Release(_ context.Context, sessionKey, _ string) error {
	f.mu.Lock()
	f.releaseCalls = append(f.releaseCalls, sessionKey)
	f.mu.Unlock()
	if f.done != nil {
		close(f.done)
	}
	return nil
}

// newAwaitingInputSession creates a real, durable conversational session
// sitting in awaiting_input — handleSteerRun's own resume branch's
// precondition — and returns its id.
func newAwaitingInputSession(t *testing.T, st *store.Store, tenantID uuid.UUID, sessionKey string) uuid.UUID {
	t.Helper()
	sessionID := uuid.New()
	digest := harness.Digest(harness.Config{SystemPromptVersion: "test"})
	if err := st.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := store.CreateSession(ctx, tx, store.Session{
			SessionID: sessionID, SessionKey: sessionKey, TenantID: tenantID,
			SurfaceID: "rest", UserID: uuid.New(), AgentVersion: 1, HarnessDigest: digest,
			DataLabel: string(provider.DataLabelInternal), RouteModelID: "fake", RouteReason: map[string]string{"r": "test"},
			AutonomyLevel: "supervised", Conversational: true,
		}); err != nil {
			return err
		}
		return store.UpdateSessionStatus(ctx, tx, sessionID, store.SessionStatusAwaitingInput, nil)
	}); err != nil {
		t.Fatalf("create awaiting_input session: %v", err)
	}
	return sessionID
}

func TestHandleSteerRun_ContendedSessionLockRefusesWithoutResuming(t *testing.T) {
	pool := setupSteerLockEnv(t)
	st := store.New(pool)
	tenantID := uuid.New()
	if err := st.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'steer-lock-test')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	sessionID := newAwaitingInputSession(t, st, tenantID, "web:peer-1")

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	runCtl := &fakeRunCtl{}
	locker := &fakeSteerLocker{acquireResults: []bool{false, false, false, false, false}}
	srv := NewServer(nil, st, crypto.NewKeyStore(kek), nil)
	srv.Verifier = fakeVerifier{wantToken: "test", tenantID: tenantID}
	srv.RunCtl = runCtl
	srv.Lock = locker

	body, _ := json.Marshal(steerRequest{Input: "go on"})
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/"+sessionID.String()+"/steer", bytes.NewReader(body))
	req.SetPathValue("id", sessionID.String())
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	srv.authed(srv.handleSteerRun).ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s, want 409 (lock contended)", rec.Code, rec.Body.String())
	}
	if runCtl.resumed {
		t.Fatal("ResumeConversation ran despite the session lock never being granted")
	}
}

func TestHandleSteerRun_LockAcquiredAndReleasedAroundResume(t *testing.T) {
	pool := setupSteerLockEnv(t)
	st := store.New(pool)
	tenantID := uuid.New()
	if err := st.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'steer-lock-test')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	sessionID := newAwaitingInputSession(t, st, tenantID, "web:peer-2")

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	runCtl := &fakeRunCtl{}
	locker := &fakeSteerLocker{acquireResults: []bool{true}, done: make(chan struct{})}
	srv := NewServer(nil, st, crypto.NewKeyStore(kek), nil)
	srv.Verifier = fakeVerifier{wantToken: "test", tenantID: tenantID}
	srv.RunCtl = runCtl
	srv.Lock = locker

	body, _ := json.Marshal(steerRequest{Input: "go on"})
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/"+sessionID.String()+"/steer", bytes.NewReader(body))
	req.SetPathValue("id", sessionID.String())
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	srv.authed(srv.handleSteerRun).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}

	select {
	case <-locker.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Release")
	}

	locker.mu.Lock()
	defer locker.mu.Unlock()
	if len(locker.acquireCalls) != 1 || locker.acquireCalls[0] != "web:peer-2" {
		t.Fatalf("Acquire calls = %v, want exactly one for %q", locker.acquireCalls, "web:peer-2")
	}
	if len(locker.releaseCalls) != 1 || locker.releaseCalls[0] != "web:peer-2" {
		t.Fatalf("Release calls = %v, want exactly one for %q", locker.releaseCalls, "web:peer-2")
	}
}
