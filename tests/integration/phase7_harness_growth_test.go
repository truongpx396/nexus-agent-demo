//go:build integration

// Phase 7 — Harness growth: memory, skills, surfaces (README.md §7). Covers
// this phase's own acceptance bars: memory injected at session start
// (task 7.1) leaves an audit trail; a skill's declared_tool_ids intersect
// the CURRENTLY resolved catalog, never a stale snapshot, and a mid-session
// revocation degrades gracefully instead of failing the run (tasks 7.4/7.8
// — the README §7 demo's own "revoke one of its declared_tool_ids... and
// activate again" scenario, driven through the REAL tools.Pipeline and a
// REAL runctl.SkillEventRecorder, not just the fakes
// internal/tools/builtin/skill_test.go already covers at the unit level);
// the outbox's at-least-once/idempotent delivery discipline (task 7.14);
// and the demo's own literal bar — REST and nexusctl produce identical
// event sequences for the same input (task 7.15).
package integration

import (
	"bytes"
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

	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/memory"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions"
	"github.com/truongpx396/nexus-agent-demo/internal/permissions/safety"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/skills"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/cli"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/internal/tools"
	"github.com/truongpx396/nexus-agent-demo/internal/tools/builtin"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// --- task 7.1: memory injected at session start ---

func TestMemory_InjectedAtSessionStart_ProducesLeadingAuditEvent(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	rig.createSession(t, sessionID, userID, "autonomous")

	memStore := &memory.Store{RootDir: t.TempDir()}
	if err := memStore.Write(rig.tenantID, "notes.md", []byte("The user prefers terse commit messages.")); err != nil {
		t.Fatalf("write memory file: %v", err)
	}
	snap, err := memStore.LoadForSession(ctx, rig.st, rig.tenantID)
	if err != nil {
		t.Fatalf("LoadForSession: %v", err)
	}
	if len(snap.SourceIDs) != 1 || snap.SourceIDs[0] != "notes.md" || !strings.Contains(snap.Text, "terse commit messages") {
		t.Fatalf("snapshot = %+v, want notes.md loaded with its content", snap)
	}

	prov := fake.New(fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "content", Text: "ok"},
		{Kind: "done", Done: "stop"},
	}})
	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{prov}),
		Tools:    kernel.NotImplementedToolExecutor{},
		Budget:   kernel.NoopBudgetGate{},
		Store:    rig.st,
	}
	sealFn, _ := rig.seal(t, sessionID)
	st0 := &kernel.RunState{TenantID: rig.tenantID, SessionID: sessionID, Seal: sealFn}
	cfg := kernel.RunConfig{
		System:        "base system prompt\n\n" + snap.Text,
		ModelID:       "fake",
		MaxTurns:      5,
		Input:         "hello",
		MemorySources: snap.SourceIDs,
		AutonomyLevel: "autonomous",
	}

	var events []store.Event
	for ev, err := range k.Run(ctx, st0, cfg) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		events = append(events, ev)
	}

	if len(events) == 0 || events[0].Type != store.EventMemoryLoaded {
		t.Fatalf("first event type = %v, want memory_loaded (injected at session start)", events)
	}
	if !hasEventType(events, store.EventTerminal) {
		t.Fatal("run never reached a terminal event")
	}
}

// --- tasks 7.4/7.8: skill activation through the REAL pipeline, a
// mid-session tool revocation fails closed and the run continues ---

func TestSkills_MidSessionToolRevocation_EmitsCapabilityIgnoredAndContinues(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	rig.createSession(t, sessionID, userID, "autonomous")
	sealFn, _ := rig.seal(t, sessionID)
	rig.seedOneEvent(t, sessionID, sealFn) // SkillEventRecorder's appendEvent needs an existing key to reuse

	fileRead := builtin.FileRead{}
	reg := tools.NewRegistry()
	if err := reg.DeclareNamespace("platform", "test"); err != nil {
		t.Fatalf("declare namespace: %v", err)
	}

	bundle := skills.SkillBundle{
		SkillID:         "triage-report",
		Description:     "Triages a weekly report.",
		DeclaredToolIDs: []string{fileRead.ID().String()},
	}
	catalog := skills.NewCatalog([]skills.SkillBundle{bundle})
	activate := builtin.ActivateSkill{
		Catalog:  catalog,
		Registry: reg,
		Events:   runctl.NewSkillEventRecorder(rig.st, rig.keys, rig.chain),
		Admitted: func(uuid.UUID) []string { return []string{"triage-report"} },
	}

	for _, tool := range []tools.Tool{fileRead, activate} {
		if err := reg.Register(tool); err != nil {
			t.Fatalf("register %s: %v", tool.ID(), err)
		}
		if err := reg.SetAdmissionStatus(tool.ID(), tools.AdmissionClean); err != nil {
			t.Fatalf("admit %s: %v", tool.ID(), err)
		}
	}
	manifest := tools.BuildManifest(reg)
	chain := permissions.NewChain(permissions.ChainConfig{
		Profiles: permissions.ProfileSet{Profiles: []permissions.ToolProfile{
			permissions.NewToolProfile("default", 1, fileRead.ID().String(), activate.ID().String()),
		}},
		Safety: safety.NewClassifier(safety.DefaultRules(), alwaysDeferModel{}, 0),
	})
	pipeline := tools.NewPipeline(tools.PipelineConfig{Registry: reg, Manifest: manifest, Chain: chain})

	inv := tools.Invocation{
		TenantID: rig.tenantID, SessionID: sessionID,
		ToolName: activate.ID().String(), Input: json.RawMessage(`{"skill_id":"triage-report"}`),
		AutonomyLevel: "autonomous",
	}

	result1 := pipeline.Execute(ctx, inv)
	if result1.IsError {
		t.Fatalf("first activation failed: %s", result1.Reason)
	}
	held1 := decodeHeldToolIDs(t, result1.Output)
	if len(held1) != 1 || held1[0] != fileRead.ID().String() {
		t.Fatalf("first activation held = %v, want [%s]", held1, fileRead.ID())
	}

	// The tenant's tool catalog moves BETWEEN activations — README §7's own
	// demo language: "revoke one of its declared_tool_ids from the tenant
	// catalog mid-session." Checked live at activation time (task 7.8), not
	// against what admission saw when the session started.
	if err := reg.SetAdmissionStatus(fileRead.ID(), tools.AdmissionFlagged); err != nil {
		t.Fatalf("revoke file_read: %v", err)
	}

	result2 := pipeline.Execute(ctx, inv)
	if result2.IsError {
		t.Fatalf("second activation failed (should degrade, not fail): %s", result2.Reason)
	}
	held2 := decodeHeldToolIDs(t, result2.Output)
	if len(held2) != 0 {
		t.Fatalf("second activation held = %v, want none — file_read was revoked", held2)
	}

	events, err := listEventsDirect(ctx, rig.st, rig.tenantID, sessionID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if !hasEventType(events, store.EventSkillCapabilityIgnored) {
		t.Fatal("expected an EventSkillCapabilityIgnored event after the mid-session revocation")
	}
	activatedCount := 0
	for _, e := range events {
		if e.Type == store.EventSkillActivated {
			activatedCount++
		}
	}
	if activatedCount != 2 {
		t.Fatalf("EventSkillActivated count = %d, want 2 (one per activation, run continues both times)", activatedCount)
	}
}

func decodeHeldToolIDs(t *testing.T, output json.RawMessage) []string {
	t.Helper()
	var out struct {
		HeldToolIDs []string `json:"held_tool_ids"`
	}
	if err := json.Unmarshal(output, &out); err != nil {
		t.Fatalf("decode held_tool_ids: %v", err)
	}
	return out.HeldToolIDs
}

// --- task 7.14: the delivery outbox ---

type flakySender struct {
	failuresRemaining int
	alwaysFail        bool
	calls             int
}

func (s *flakySender) Send(context.Context, string, string, []byte) error {
	s.calls++
	if s.alwaysFail {
		return fmt.Errorf("simulated permanent failure")
	}
	if s.failuresRemaining > 0 {
		s.failuresRemaining--
		return fmt.Errorf("simulated transient failure")
	}
	return nil
}

func TestOutbox_RetriesThenDeliversAndStaysIdempotent(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	rig.createSession(t, sessionID, userID, "autonomous")
	sealFn, _ := rig.seal(t, sessionID)
	rig.seedOneEvent(t, sessionID, sealFn)

	ob := &surfaces.Outbox{Store: rig.st, Keys: rig.keys, Chain: rig.chain}
	sender := &flakySender{failuresRemaining: 2}

	for i := 0; i < 2; i++ {
		if err := ob.Deliver(ctx, rig.tenantID, sessionID, 1, "rest", "operator", []byte("hi"), sender); err != nil {
			t.Fatalf("Deliver attempt %d: %v", i+1, err)
		}
	}
	if sender.calls != 2 {
		t.Fatalf("sender.calls = %d after 2 failing attempts, want 2", sender.calls)
	}

	if err := ob.Deliver(ctx, rig.tenantID, sessionID, 1, "rest", "operator", []byte("hi"), sender); err != nil {
		t.Fatalf("Deliver attempt 3: %v", err)
	}
	if sender.calls != 3 {
		t.Fatalf("sender.calls = %d after the successful attempt, want 3", sender.calls)
	}

	// Idempotent: a repeat call against an already-delivered key must never
	// send again.
	if err := ob.Deliver(ctx, rig.tenantID, sessionID, 1, "rest", "operator", []byte("hi"), sender); err != nil {
		t.Fatalf("Deliver after delivered: %v", err)
	}
	if sender.calls != 3 {
		t.Fatalf("sender.calls = %d after a repeat call on a delivered key, want still 3 (no duplicate send)", sender.calls)
	}

	events, err := listEventsDirect(ctx, rig.st, rig.tenantID, sessionID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if !hasEventType(events, store.EventDeliveryEnqueued) || !hasEventType(events, store.EventDeliveryFailed) || !hasEventType(events, store.EventDeliveryDelivered) {
		t.Fatal("expected enqueued, failed, and delivered events across the retry sequence")
	}
}

func TestOutbox_FailsPermanentlyAfterCapAndNeverSendsAgain(t *testing.T) {
	rig := newPhase6Rig(t)
	ctx := context.Background()
	sessionID, userID := uuid.New(), uuid.New()
	rig.createSession(t, sessionID, userID, "autonomous")
	sealFn, _ := rig.seal(t, sessionID)
	rig.seedOneEvent(t, sessionID, sealFn)

	ob := &surfaces.Outbox{Store: rig.st, Keys: rig.keys, Chain: rig.chain}
	sender := &flakySender{alwaysFail: true}

	for i := 0; i < 3; i++ {
		if err := ob.Deliver(ctx, rig.tenantID, sessionID, 1, "rest", "operator", []byte("hi"), sender); err != nil {
			t.Fatalf("Deliver attempt %d: %v", i+1, err)
		}
	}

	var status string
	err := rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM deliveries WHERE session_id = $1 AND seq = $2`, sessionID, int64(1)).Scan(&status)
	})
	if err != nil {
		t.Fatalf("query delivery status: %v", err)
	}
	if status != string(store.DeliveryFailedPermanent) {
		t.Fatalf("delivery status = %q after %d attempts, want failed_permanent", status, 3)
	}

	callsBefore := sender.calls
	if err := ob.Deliver(ctx, rig.tenantID, sessionID, 1, "rest", "operator", []byte("hi"), sender); err != nil {
		t.Fatalf("Deliver after cap: %v", err)
	}
	if sender.calls != callsBefore {
		t.Fatalf("sender.calls grew from %d to %d after the retry cap — failed_permanent must refuse further sends", callsBefore, sender.calls)
	}

	// A different (session, seq) that was never attempted stays pending —
	// distinguishable from this one's failed_permanent (task 7.14's own
	// wording).
	if err := rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := store.OpenOrFindDelivery(ctx, tx, rig.tenantID, sessionID, 2, "rest", "operator")
		return err
	}); err != nil {
		t.Fatalf("open a fresh delivery: %v", err)
	}
	var pendingStatus string
	err = rig.st.InTenantTx(ctx, rig.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM deliveries WHERE session_id = $1 AND seq = $2`, sessionID, int64(2)).Scan(&pendingStatus)
	})
	if err != nil {
		t.Fatalf("query fresh delivery status: %v", err)
	}
	if pendingStatus != string(store.DeliveryPending) {
		t.Fatalf("fresh delivery status = %q, want pending", pendingStatus)
	}
}

// --- task 7.15: REST and nexusctl produce identical event sequences ---

func TestRESTAndCLI_ProduceIdenticalEventSequencesAndTerminalReason(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID, userID := uuid.New(), uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("generate KEK: %v", err)
	}
	keyStore := crypto.NewKeyStore(kek)

	newScript := func() fake.Script {
		return fake.Script{Chunks: []fake.ChunkSpec{
			{Kind: "content", Text: "All done."},
			{Kind: "usage", InputUncached: 10, OutputTokens: 5},
			{Kind: "done", Done: "stop"},
		}}
	}
	prov := fake.New(newScript(), newScript()) // one identical script per run below

	starter := &testRunStarter{kernel: &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{prov}),
		Tools:    kernel.NotImplementedToolExecutor{},
		Budget:   kernel.NoopBudgetGate{},
		Store:    st,
	}}
	srv := rest.NewServer(starter, st, keyStore, nil)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	restRunID := postRunViaREST(t, httpSrv, tenantID, userID, "do the thing")
	restTypes := waitForTerminalEventTypes(t, ctx, st, tenantID, restRunID)

	t.Setenv("NEXUS_HTTP_ADDR", httpSrv.URL)
	t.Setenv("NEXUS_TENANT_ID", tenantID.String())
	t.Setenv("NEXUS_USER_ID", userID.String())
	var out, errOut bytes.Buffer
	if code := cli.Main([]string{"run", "do the thing"}, &out, &errOut); code != 0 {
		t.Fatalf("cli.Main exit code = %d (stderr: %s)", code, errOut.String())
	}
	cliRunID := extractRunID(t, out.String())
	cliTypes := waitForTerminalEventTypes(t, ctx, st, tenantID, cliRunID)

	if len(restTypes) != len(cliTypes) {
		t.Fatalf("event count differs: rest=%v cli=%v", restTypes, cliTypes)
	}
	for i := range restTypes {
		if restTypes[i] != cliTypes[i] {
			t.Fatalf("event sequence differs at index %d: rest=%s cli=%s\nrest=%v\ncli=%v", i, restTypes[i], cliTypes[i], restTypes, cliTypes)
		}
	}
}

func postRunViaREST(t *testing.T, httpSrv *httptest.Server, tenantID, userID uuid.UUID, input string) uuid.UUID {
	t.Helper()
	body, err := json.Marshal(map[string]string{"input": input})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/runs", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Nexus-Tenant-ID", tenantID.String())
	req.Header.Set("X-Nexus-User-ID", userID.String())
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /v1/runs: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/runs status = %d, want 202", resp.StatusCode)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return uuid.MustParse(out.RunID)
}

func extractRunID(t *testing.T, output string) uuid.UUID {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if id, ok := strings.CutPrefix(line, "run_id: "); ok {
			return uuid.MustParse(strings.TrimSpace(id))
		}
	}
	t.Fatalf("no run_id line found in nexusctl output: %s", output)
	return uuid.Nil
}

func waitForTerminalEventTypes(t *testing.T, ctx context.Context, st *store.Store, tenantID, sessionID uuid.UUID) []store.EventType {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, err := listEventsDirect(ctx, st, tenantID, sessionID)
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		if hasEventType(events, store.EventTerminal) {
			types := make([]store.EventType, len(events))
			for i, e := range events {
				types[i] = e.Type
			}
			return types
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s never reached a terminal event within the deadline", sessionID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
