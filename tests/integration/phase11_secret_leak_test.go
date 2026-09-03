//go:build integration

// README Phase 11's own acceptance criterion, verbatim: "a test that greps
// every event payload, log line, and span for a live OAuth token — none
// ever appears (the same class of test #34 runs for telemetry
// [internal/obs/allowlist_test.go], aimed at a new secret class)." This is
// that test, run against the real tool pipeline (internal/tools.Pipeline,
// not a hand-rolled stand-in) so what it proves is the actual dispatch path
// a live run takes, not merely internal/connectors' own unit-level
// contract (already covered by internal/connectors/oauth_integration_test.go).
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/truongpx396/nexus-agent-demo/internal/connectors"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/hooks"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions/safety"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
	"github.com/truongpx396/nexus-agent-demo/internal/tools/builtin"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// liveTestToken is the one secret value this test tracks end to end —
// distinctive enough (a real token would never collide with it) that a
// grep match is unambiguous.
const liveTestToken = "LIVE-OAUTH-TOKEN-do-not-leak-8f3e9c21" //nolint:gosec // a fake fixture value this test asserts never leaks, not a real credential

// alwaysDeferModel is tests/integration/phase6_reliability_test.go's own
// (package-level, reused here rather than redeclared).

// setupSecretLeakEnv is this file's own postgres-only rig (no PgBouncer,
// no Redis — internal/connectors.Vault.AccessToken never touches Redis,
// only BeginAuth/HandleCallback do), the same minimal-services-needed
// tradeoff internal/crypto/keystore_integration_test.go's own doc comment
// explains.
func setupSecretLeakEnv(t *testing.T) *pgxpool.Pool {
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

func TestSecretLeak_LiveOAuthTokenNeverAppearsInOutputOrLogs(t *testing.T) {
	pool := setupSecretLeakEnv(t)
	s := store.New(pool)
	tenantID, userID := uuid.New(), uuid.New()

	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'secret-leak-test')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	// The provider API this connector call hits — echoes nothing back that
	// would itself leak the token (a real API wouldn't either); this
	// server's own request-side check is what the assertions below rely on
	// having actually exercised the real Authorization header.
	var sawAuthHeader string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthHeader = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer apiSrv.Close()

	// Seed the vaulted connection directly (the authorization-code flow
	// itself is covered end to end by
	// internal/connectors/oauth_integration_test.go) — sealed under the
	// tenant's own DEK, exactly like a real HandleCallback would persist it.
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	keyStore := crypto.NewKeyStore(kek)
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		dek, err := keyStore.NewDEK(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		sealed, err := crypto.Seal(dek, []byte(liveTestToken), tenantID.String(), userID.String()+"|acme")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO oauth_connections (connection_id, tenant_id, user_id, provider, sealed_access_token, key_id, updated_at)
			 VALUES ($1, $2, $3, 'acme', $4, $5, now())`,
			uuid.New(), tenantID, userID, sealed, dek.KeyID,
		)
		return err
	}); err != nil {
		t.Fatalf("seed oauth_connections: %v", err)
	}

	vault := &connectors.Vault{Store: s, Keys: keyStore, Providers: connectors.NewRegistry(connectors.Provider{Name: "acme"})}
	tool := builtin.ConnectorFetch{
		Tokens:               vault,
		Sessions:             fakeSessionLookup{userID: userID},
		AllowedHosts:         []string{"*"},
		AllowPrivateNetworks: true,
	}

	// Capture EVERY slog record for the duration of this call — a
	// TextHandler renders the full attribute set to text, exactly what a
	// real log sink would persist.
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	reg := tools.NewRegistry()
	if err := reg.DeclareNamespace("platform", "test"); err != nil {
		t.Fatalf("DeclareNamespace: %v", err)
	}
	if err := reg.Register(tool); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.SetAdmissionStatus(tool.ID(), tools.AdmissionClean); err != nil {
		t.Fatalf("SetAdmissionStatus: %v", err)
	}
	manifest := tools.BuildManifest(reg)
	chain := permissions.NewChain(permissions.ChainConfig{
		Profiles: permissions.ProfileSet{Profiles: []permissions.ToolProfile{permissions.NewToolProfile("default", 1, tool.ID().String())}},
		Safety:   safety.NewClassifier(safety.DefaultRules(), alwaysDeferModel{}, 0),
	})
	pipeline := tools.NewPipeline(tools.PipelineConfig{
		Registry: reg, Manifest: manifest, Chain: chain, Hooks: hooks.NewDispatcher(),
		Blobs: tools.BlobStore{Dir: t.TempDir()},
	})

	input, err := json.Marshal(map[string]string{"provider": "acme", "url": apiSrv.URL})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	inv := tools.Invocation{
		TenantID: tenantID, SessionID: uuid.New(), ToolName: tool.ID().String(),
		Input: input, AutonomyLevel: "autonomous",
	}

	// ConnectorFetch honestly declares ALL three Rule-of-Two legs at once
	// (it reads a private vaulted credential AND communicates externally
	// AND returns untrusted content) — engaging three legs in one call is
	// exactly what Rule of Two exists to catch, so the first call in a
	// fresh session correctly suspends for approval rather than running.
	// ExecuteApproved (the resume-after-a-human-said-yes path) is what
	// actually dispatches Tool.Call — the same path a real approval grant
	// takes, and the one this test needs to reach the code that touches
	// the live token.
	asked := pipeline.Execute(context.Background(), inv)
	if !asked.AwaitingApproval {
		t.Fatalf("Execute() = %+v, want AwaitingApproval (Rule of Two: 3 legs at once)", asked)
	}
	result := pipeline.ExecuteApproved(context.Background(), inv, asked.CanonicalDigest)
	if result.IsError {
		t.Fatalf("ExecuteApproved() = %+v, want success", result)
	}

	// The property that actually matters (README's own acceptance
	// criterion): the token must have genuinely been used (the API server
	// saw it)...
	if sawAuthHeader != "Bearer "+liveTestToken {
		t.Fatalf("api server saw Authorization %q, want the real bearer token — the test didn't exercise what it claims to", sawAuthHeader)
	}
	// ...yet it must never appear in the tool_result this becomes (what
	// the model sees, and what a kernel run would seal into an event
	// payload)...
	if strings.Contains(string(result.Output), liveTestToken) {
		t.Fatalf("ExecuteResult.Output leaked the live token: %s", result.Output)
	}
	// ...nor in anything logged during the whole call.
	if strings.Contains(logBuf.String(), liveTestToken) {
		t.Fatalf("logs leaked the live token:\n%s", logBuf.String())
	}
}

type fakeSessionLookup struct{ userID uuid.UUID }

func (f fakeSessionLookup) UserIDForSession(context.Context, uuid.UUID, uuid.UUID) (uuid.UUID, error) {
	return f.userID, nil
}
