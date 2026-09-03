//go:build integration

package mcp

import (
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
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// setupMCPEnv mirrors internal/crypto/keystore_integration_test.go's own
// setupKeystoreEnv (this codebase's established per-file duplication idiom
// for integration test scaffolding) — one postgres container, migrated as
// the superuser, connected at runtime as the RLS-restricted nexus_app role.
func setupMCPEnv(t *testing.T) *pgxpool.Pool {
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
	pgC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: pgReq,
		Started:          true,
	})
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

// fakeMCPServer serves a minimal JSON-RPC 2.0 tools/list + tools/call pair
// over HTTP — enough of the Streamable HTTP transport for Client/Resolver
// to exercise against, without a real MCP SDK dependency.
func fakeMCPServer(t *testing.T, wantBearer string, tool Tool, callHandler func(name string, args json.RawMessage) CallResult) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantBearer != "" && r.Header.Get("authorization") != "Bearer "+wantBearer {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("content-type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []Tool{tool}}})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			result := callHandler(p.Name, p.Arguments)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		default:
			http.Error(w, "unknown method", http.StatusNotImplemented)
		}
	}))
}

func insertTenant(t *testing.T, s *store.Store, tenantID uuid.UUID) {
	t.Helper()
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, 'mcp-test')`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
}

func insertAdmittedServer(t *testing.T, s *store.Store, tenantID uuid.UUID, name, baseURL string) {
	t.Helper()
	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO mcp_servers (server_id, tenant_id, name, base_url, auth_kind, status) VALUES ($1, $2, $3, $4, 'none', 'admitted')`,
			uuid.New(), tenantID, name, baseURL,
		)
		return err
	}); err != nil {
		t.Fatalf("insert admitted server: %v", err)
	}
}

func TestResolver_UnadmittedServerRefusesResolution(t *testing.T) {
	pool := setupMCPEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	insertTenant(t, s, tenantID)
	// No mcp_servers row at all — the tenant never admitted anything.

	r := &Resolver{Store: s, AllowedHosts: []string{"*"}}
	ref := tools.ToolRef{Namespace: "mcp/nonexistent-server", Name: "some_tool", Version: "v12345678"}
	tool, ok, err := r.Resolve(context.Background(), tenantID, uuid.New(), ref)
	if err != nil {
		t.Fatalf("Resolve error = %v", err)
	}
	if ok || tool != nil {
		t.Fatalf("Resolve against an unadmitted server = (%v, %v), want (nil, false)", tool, ok)
	}
}

func TestResolver_HappyPath_ResolvesAndCallsThroughAdapter(t *testing.T) {
	pool := setupMCPEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	insertTenant(t, s, tenantID)

	remoteTool := Tool{Name: "create_issue", Description: "opens an issue", InputSchema: json.RawMessage(`{"type":"object"}`)}
	var calledWith json.RawMessage
	srv := fakeMCPServer(t, "", remoteTool, func(name string, args json.RawMessage) CallResult {
		calledWith = args
		return CallResult{Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "text", Text: "issue #42 created"}}}
	})
	defer srv.Close()

	insertAdmittedServer(t, s, tenantID, "github", srv.URL)

	r := &Resolver{Store: s, AllowedHosts: []string{"*"}}
	ref := tools.ToolRef{Namespace: "mcp/github", Name: "create_issue", Version: "v" + remoteTool.SchemaDigest()}
	tool, ok, err := r.Resolve(context.Background(), tenantID, uuid.New(), ref)
	if err != nil {
		t.Fatalf("Resolve error = %v", err)
	}
	if !ok || tool == nil {
		t.Fatalf("Resolve() = (%v, %v), want a resolved tool", tool, ok)
	}
	if tool.ID() != ref {
		t.Fatalf("resolved tool ID = %+v, want %+v", tool.ID(), ref)
	}
	// A dynamically-resolved MCP tool is never trusted narrower than the
	// fail-closed floor (README task 11.1) regardless of what the server's
	// own description claims.
	if taint := tool.Taint(); !taint.ReturnsUntrusted || !taint.ReadsPrivateData || !taint.MutatesExternal {
		t.Fatalf("Taint() = %+v, want the fail-closed default (all true)", taint)
	}

	result, err := tool.Call(context.Background(), json.RawMessage(`{"title":"bug"}`), tools.RunContext{TenantID: tenantID})
	if err != nil {
		t.Fatalf("Call error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Call() = %+v, want success", result)
	}
	if string(calledWith) != `{"title":"bug"}` {
		t.Fatalf("remote server saw arguments %s, want the tool's own input echoed through unmodified", calledWith)
	}
}

func TestResolver_SchemaDriftRefusesTheStaleRef(t *testing.T) {
	pool := setupMCPEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	insertTenant(t, s, tenantID)

	remoteTool := Tool{Name: "create_issue", Description: "opens an issue", InputSchema: json.RawMessage(`{"type":"object"}`)}
	srv := fakeMCPServer(t, "", remoteTool, func(string, json.RawMessage) CallResult { return CallResult{} })
	defer srv.Close()
	insertAdmittedServer(t, s, tenantID, "github", srv.URL)

	r := &Resolver{Store: s, AllowedHosts: []string{"*"}}
	// A ref pinned to a digest that no longer matches the server's CURRENT
	// schema (README task 11.1's #15 "digest re-verification at use") — the
	// server and tool NAME both still exist, but this specific qualified
	// identity does not.
	stale := tools.ToolRef{Namespace: "mcp/github", Name: "create_issue", Version: "vdeadbeef"}
	tool, ok, err := r.Resolve(context.Background(), tenantID, uuid.New(), stale)
	if err != nil {
		t.Fatalf("Resolve error = %v", err)
	}
	if ok || tool != nil {
		t.Fatalf("Resolve with a stale schema digest = (%v, %v), want (nil, false)", tool, ok)
	}
}

func TestResolver_BearerStaticAuthIsInjected(t *testing.T) {
	pool := setupMCPEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	insertTenant(t, s, tenantID)

	remoteTool := Tool{Name: "whoami", Description: "", InputSchema: json.RawMessage(`{}`)}
	srv := fakeMCPServer(t, "static-secret-123", remoteTool, func(string, json.RawMessage) CallResult { return CallResult{} })
	defer srv.Close()

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	keys := crypto.NewKeyStore(kek)

	if err := s.InTenantTx(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		dek, err := keys.NewDEK(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		aad := tenantID.String() + "|mcp|github"
		sealed, err := crypto.Seal(dek, []byte("static-secret-123"), tenantID.String(), aad)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO mcp_servers (server_id, tenant_id, name, base_url, auth_kind, sealed_static_token, key_id, status)
			 VALUES ($1, $2, 'github', $3, 'bearer_static', $4, $5, 'admitted')`,
			uuid.New(), tenantID, srv.URL, sealed, dek.KeyID,
		)
		return err
	}); err != nil {
		t.Fatalf("insert bearer_static server: %v", err)
	}

	r := &Resolver{Store: s, Keys: keys, AllowedHosts: []string{"*"}}
	ref := tools.ToolRef{Namespace: "mcp/github", Name: "whoami", Version: "v" + remoteTool.SchemaDigest()}
	tool, ok, err := r.Resolve(context.Background(), tenantID, uuid.New(), ref)
	if err != nil {
		t.Fatalf("Resolve error = %v (a wrong/missing bearer token would surface as a 401 here)", err)
	}
	if !ok || tool == nil {
		t.Fatalf("Resolve() = (%v, %v), want a resolved tool — the sealed static token must round-trip to the correct Authorization header", tool, ok)
	}
}

// TestResolver_EgressDeniedHostRefusesEvenForAnAdmittedServer proves task
// 11.9's own point: admission through mcp_servers is necessary but not
// sufficient — a server whose base_url host isn't on the SAME allowlist
// WebFetch/ConnectorFetch already enforce is refused, fail closed, never
// reached.
func TestResolver_EgressDeniedHostRefusesEvenForAnAdmittedServer(t *testing.T) {
	pool := setupMCPEnv(t)
	s := store.New(pool)
	tenantID := uuid.New()
	insertTenant(t, s, tenantID)

	remoteTool := Tool{Name: "create_issue", Description: "", InputSchema: json.RawMessage(`{}`)}
	srv := fakeMCPServer(t, "", remoteTool, func(string, json.RawMessage) CallResult { return CallResult{} })
	defer srv.Close()
	insertAdmittedServer(t, s, tenantID, "github", srv.URL)

	r := &Resolver{Store: s, AllowedHosts: []string{"some-other-host.example.com"}}
	ref := tools.ToolRef{Namespace: "mcp/github", Name: "create_issue", Version: "v" + remoteTool.SchemaDigest()}
	tool, ok, err := r.Resolve(context.Background(), tenantID, uuid.New(), ref)
	if err == nil {
		t.Fatal("Resolve against a non-allowlisted host succeeded, want an egress_denied error")
	}
	if ok || tool != nil {
		t.Fatalf("Resolve() = (%v, %v), want (nil, false) alongside the error", tool, ok)
	}
}
