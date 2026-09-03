//go:build integration

package telegram

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

// setupTelegramEnv mirrors internal/crypto/keystore_integration_test.go's
// own setupKeystoreEnv (this codebase's established per-file duplication
// idiom for integration test scaffolding).
func setupTelegramEnv(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	pgReq := testcontainers.ContainerRequest{
		Image:        "postgres:17",
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

func TestHandleWebhook_ValidUpdateCreatesARealSessionAndStartsARun(t *testing.T) {
	pool := setupTelegramEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'telegram-test')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}

	starter := &fakeStarter{}
	srv := &Server{
		Store:    s,
		KeyStore: crypto.NewKeyStore(kek),
		Starter:  starter,
		Channels: fakeChannels{secret: "s", ok: true},
	}

	body, err := json.Marshal(map[string]any{
		"update_id": 1,
		"message": map[string]any{
			"message_id": 1,
			"from":       map[string]any{"id": 42},
			"chat":       map[string]any{"id": 999},
			"text":       "please help",
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/telegram/"+tenantID.String(), bytes.NewReader(body))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "s")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	if !starter.started {
		t.Fatal("a valid update did not reach StartRun")
	}
	if starter.req.Input != "please help" {
		t.Fatalf("RunRequest.Input = %q, want the message text", starter.req.Input)
	}
	if starter.req.TenantID != tenantID {
		t.Fatalf("RunRequest.TenantID = %s, want %s", starter.req.TenantID, tenantID)
	}

	// The session this handler created must actually be durable and
	// tagged with this surface's own id — SurfaceID:"telegram" is what
	// lets a later audit/dashboard query attribute the run correctly.
	var surfaceID string
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT surface_id FROM sessions WHERE session_id = $1`, starter.req.SessionID).Scan(&surfaceID)
	}); err != nil {
		t.Fatalf("load created session: %v", err)
	}
	if surfaceID != "telegram" {
		t.Fatalf("sessions.surface_id = %q, want %q", surfaceID, "telegram")
	}
}
