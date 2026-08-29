//go:build integration

// This is the Phase 2 candidate tests/integration/README.md names: a run
// through the REST surface end to end, against a REAL postgres + pgbouncer
// (transaction-pooling) pair via testcontainers — exactly the topology
// nexusd runs against in production, per README.md §7's "isolation test
// must run through the same pooling tier production uses" rule extended to
// every integration test in this directory, not just the isolation one.
//
// It proves the two things README.md §5's Phase 2 acceptance line asks for:
// a terminal event reaches the client over SSE, and every tool_use the run
// produced ends up with exactly one paired tool_result in the durable log —
// exercised here through the stubbed Phase 2 tool executor
// (kernel.NotImplementedToolExecutor), which still has to produce a
// synthetic, correctly paired result for the invariant to hold.
package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/kernel"
	"github.com/truongpx396/nexus-agent-demo/migrations"
)

// setupPostgresAndPgBouncer starts postgres + pgbouncer (transaction
// pooling) on a shared Docker network and returns a pgxpool connected
// THROUGH pgbouncer as nexus_app, plus a direct migration pool. Deliberately
// not shared code with internal/store/isolation_integration_test.go's
// setupIsolationEnv: that helper is unexported (white-box, needs
// scopeTenant), and this is a black-box test in a different package — see
// tests/integration/README.md.
func setupPostgresAndPgBouncer(t *testing.T) (appPool *pgxpool.Pool, cleanup func()) {
	t.Helper()
	ctx := context.Background()

	nw, err := tcnetwork.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}

	pgReq := testcontainers.ContainerRequest{
		Image:        "postgres:17",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "nexus",
			"POSTGRES_PASSWORD": "nexus",
			"POSTGRES_DB":       "nexus",
		},
		Networks:       []string{nw.Name},
		NetworkAliases: map[string][]string{nw.Name: {"postgres"}},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(120 * time.Second),
	}
	pgC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: pgReq, Started: true})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}

	pbReq := testcontainers.ContainerRequest{
		Image:        "edoburu/pgbouncer:latest",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"DATABASE_URL":      "postgres://nexus:nexus@postgres:5432/nexus",
			"POOL_MODE":         "transaction",
			"AUTH_TYPE":         "scram-sha-256",
			"MAX_CLIENT_CONN":   "200",
			"DEFAULT_POOL_SIZE": "20",
		},
		Networks:   []string{nw.Name},
		WaitingFor: wait.ForLog("process up").WithStartupTimeout(60 * time.Second),
	}
	pbC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: pbReq, Started: true})
	if err != nil {
		t.Fatalf("start pgbouncer container: %v", err)
	}

	pgHost, err := pgC.Host(ctx)
	if err != nil {
		t.Fatalf("postgres host: %v", err)
	}
	pgPort, err := pgC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("postgres mapped port: %v", err)
	}
	pbHost, err := pbC.Host(ctx)
	if err != nil {
		t.Fatalf("pgbouncer host: %v", err)
	}
	pbPort, err := pbC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("pgbouncer mapped port: %v", err)
	}

	migrateDSN := fmt.Sprintf("postgres://nexus:nexus@%s:%s/nexus", pgHost, pgPort.Port())
	appDSN := fmt.Sprintf("postgres://nexus_app:nexus_app@%s:%s/nexus", pbHost, pbPort.Port())

	migratePool, err := pgxpool.New(ctx, migrateDSN)
	if err != nil {
		t.Fatalf("connect direct to postgres: %v", err)
	}
	if _, err := store.Migrate(ctx, migratePool, migrations.FS); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	appPool, err = pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect through pgbouncer: %v", err)
	}

	cleanup = func() {
		appPool.Close()
		migratePool.Close()
		_ = pbC.Terminate(ctx)
		_ = pgC.Terminate(ctx)
		_ = nw.Remove(ctx)
	}
	return appPool, cleanup
}

// testRunStarter mirrors cmd/nexusd/main.go's kernelRunStarter — the only
// place a kernel.RunState/kernel.RunConfig may be constructed from a
// rest.RunRequest, per internal/surfaces/rest's boundary rule
// (tests/contract/boundaries_test.go). Duplicating this small adapter here
// rather than exporting it from cmd/nexusd is deliberate: cmd/ is a binary
// entrypoint, not a library other packages should import.
type testRunStarter struct {
	kernel *kernel.Kernel
}

func (a *testRunStarter) StartRun(_ context.Context, req rest.RunRequest) (<-chan rest.RunEvent, error) {
	st := &kernel.RunState{TenantID: req.TenantID, SessionID: req.SessionID, Seal: kernel.SealFunc(req.Seal)}
	cfg := kernel.RunConfig{System: "You are a helpful agent.", ModelID: req.ModelID, MaxTurns: 10, Input: req.Input}

	ch := make(chan rest.RunEvent, 8)
	go func() {
		defer close(ch)
		for ev, err := range a.kernel.Run(context.Background(), st, cfg) {
			ch <- rest.RunEvent{Event: ev, Err: err}
			if err != nil {
				return
			}
		}
	}()
	return ch, nil
}

func insertTenant(ctx context.Context, s *store.Store, tenantID uuid.UUID) error {
	return s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (tenant_id, name) VALUES ($1, $2)`, tenantID, "integration-test-tenant")
		return err
	})
}

func listEventsDirect(ctx context.Context, s *store.Store, tenantID, sessionID uuid.UUID) ([]store.Event, error) {
	var events []store.Event
	err := s.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var lerr error
		events, lerr = store.ListEvents(ctx, tx, sessionID)
		return lerr
	})
	return events, err
}

type sseFrame struct {
	Event string
	Data  json.RawMessage
}

// readSSEFrames reads every "event: ...\ndata: ...\n\n" frame from r until
// EOF (handleEvents itself closes the connection once it has written a
// terminal event — see server.go — so a clean read to EOF is the expected,
// not a hung, ending).
func readSSEFrames(t *testing.T, r *bufio.Reader) []sseFrame {
	t.Helper()
	var frames []sseFrame
	var event string
	var data strings.Builder
	for {
		line, err := r.ReadString('\n')
		trimmed := strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(trimmed, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		case strings.HasPrefix(trimmed, "data:"):
			data.WriteString(strings.TrimPrefix(trimmed, "data:"))
		case trimmed == "" && event != "":
			frames = append(frames, sseFrame{Event: event, Data: json.RawMessage(data.String())})
			event, data = "", strings.Builder{}
		}
		if err != nil {
			return frames
		}
	}
}

type dtoBody struct {
	Reason string `json:"reason,omitempty"`
}

type eventDTOShape struct {
	EventID string   `json:"event_id"`
	Type    string   `json:"type"`
	PairRef *string  `json:"pair_ref,omitempty"`
	Body    *dtoBody `json:"body,omitempty"`
}

func TestRESTRunEndToEnd(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID := uuid.New()
	userID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("generate KEK: %v", err)
	}
	keyStore := crypto.NewKeyStore(kek)

	// Turn 1: a tool call (exercises the stub ToolExecutor's synthetic
	// paired result over the wire). Turn 2: natural completion.
	fakeProvider := fake.New(
		fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "tool_use", ToolUseID: "tu1", ToolName: "demo_tool", Input: `{}`},
			{Kind: "done", Done: "stop"},
		}},
		fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "content", Text: "All done."},
			{Kind: "done", Done: "stop"},
		}},
	)

	starter := &testRunStarter{kernel: &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fakeProvider}),
		Tools:    kernel.NotImplementedToolExecutor{},
		Budget:   kernel.NoopBudgetGate{},
		Store:    st,
	}}

	srv := rest.NewServer(starter, st, keyStore, nil)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	client := httpSrv.Client()

	// POST /v1/runs
	createReq, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/runs", strings.NewReader(`{"input":"do the thing"}`))
	createReq.Header.Set("X-Nexus-Tenant-ID", tenantID.String())
	createReq.Header.Set("X-Nexus-User-ID", userID.String())
	createReq.Header.Set("content-type", "application/json")
	createResp, err := client.Do(createReq)
	if err != nil {
		t.Fatalf("POST /v1/runs: %v", err)
	}
	defer createResp.Body.Close() //nolint:errcheck // read-only response body
	if createResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/runs status = %d, want 202", createResp.StatusCode)
	}
	var created struct {
		RunID string `json:"run_id"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	// GET /v1/runs/{id}/events (SSE) — audience check with the WRONG user
	// must be refused before we even try the real one.
	wrongReq, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/v1/runs/"+created.RunID+"/events", nil)
	wrongReq.Header.Set("X-Nexus-Tenant-ID", tenantID.String())
	wrongReq.Header.Set("X-Nexus-User-ID", uuid.New().String())
	wrongResp, err := client.Do(wrongReq)
	if err != nil {
		t.Fatalf("GET events (wrong user): %v", err)
	}
	_ = wrongResp.Body.Close()
	if wrongResp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET events with the wrong user = %d, want 403", wrongResp.StatusCode)
	}

	eventsReq, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/v1/runs/"+created.RunID+"/events", nil)
	eventsReq.Header.Set("X-Nexus-Tenant-ID", tenantID.String())
	eventsReq.Header.Set("X-Nexus-User-ID", userID.String())
	eventsResp, err := client.Do(eventsReq)
	if err != nil {
		t.Fatalf("GET /v1/runs/{id}/events: %v", err)
	}
	defer eventsResp.Body.Close() //nolint:errcheck // read-only response body
	if eventsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET events status = %d, want 200", eventsResp.StatusCode)
	}

	frames := readSSEFrames(t, bufio.NewReader(eventsResp.Body))
	if len(frames) == 0 {
		t.Fatal("no SSE frames received")
	}

	toolUseIDs := map[string]bool{}
	pairedResultFor := map[string]int{}
	var sawTerminal bool
	var terminalReason string
	for _, f := range frames {
		var dto eventDTOShape
		if err := json.Unmarshal(f.Data, &dto); err != nil {
			t.Fatalf("unmarshal SSE frame %q: %v (data=%s)", f.Event, err, f.Data)
		}
		switch dto.Type {
		case "tool_use":
			toolUseIDs[dto.EventID] = true
		case "tool_result":
			if dto.PairRef == nil {
				t.Fatalf("tool_result event %s has no pair_ref", dto.EventID)
			}
			pairedResultFor[*dto.PairRef]++
		case "terminal":
			sawTerminal = true
			if dto.Body != nil {
				terminalReason = dto.Body.Reason
			}
		}
	}

	if !sawTerminal {
		t.Fatal("no terminal event arrived over SSE")
	}
	if terminalReason != "completed" {
		t.Fatalf("terminal reason = %q, want %q", terminalReason, "completed")
	}
	if len(toolUseIDs) == 0 {
		t.Fatal("expected at least one tool_use event from the scripted turn")
	}
	for id := range toolUseIDs {
		if pairedResultFor[id] != 1 {
			t.Fatalf("tool_use %s has %d paired tool_results over SSE, want exactly 1", id, pairedResultFor[id])
		}
	}

	// And independently, straight from the durable log (not just what SSE
	// happened to forward): the paired-result invariant must hold there
	// too — this is the actual acceptance criterion (README.md §5, Phase 2).
	events, err := listEventsDirect(ctx, st, tenantID, uuid.MustParse(created.RunID))
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	toolUseEventIDs := map[uuid.UUID]bool{}
	resultCount := map[uuid.UUID]int{}
	for _, e := range events {
		switch e.Type { //nolint:exhaustive // only tool_use/tool_result matter to the paired-result assertion below
		case store.EventToolUse:
			toolUseEventIDs[e.EventID] = true
		case store.EventToolResult:
			if e.PairRef != nil {
				resultCount[*e.PairRef]++
			}
		}
	}
	if len(toolUseEventIDs) == 0 {
		t.Fatal("expected at least one tool_use event in the durable log")
	}
	for id := range toolUseEventIDs {
		if resultCount[id] != 1 {
			t.Fatalf("durable log: tool_use %s has %d paired tool_results, want exactly 1", id, resultCount[id])
		}
	}

	// GET /v1/runs/{id} agrees with what SSE showed.
	getReq, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/v1/runs/"+created.RunID, nil)
	getReq.Header.Set("X-Nexus-Tenant-ID", tenantID.String())
	getReq.Header.Set("X-Nexus-User-ID", userID.String())
	getResp, err := client.Do(getReq)
	if err != nil {
		t.Fatalf("GET /v1/runs/{id}: %v", err)
	}
	defer getResp.Body.Close() //nolint:errcheck // read-only response body
	var got struct {
		Status         string  `json:"status"`
		TerminalReason *string `json:"terminal_reason"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got.Status != store.SessionStatusCompleted {
		t.Fatalf("session status = %q, want %q", got.Status, store.SessionStatusCompleted)
	}
	if got.TerminalReason == nil || *got.TerminalReason != "completed" {
		t.Fatalf("session terminal_reason = %v, want \"completed\"", got.TerminalReason)
	}
}
