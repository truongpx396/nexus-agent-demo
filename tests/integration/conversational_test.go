//go:build integration

// Conversational sessions (kernel.RunConfig.Conversational,
// migrations/0023_conversational_sessions.sql) are the real, backend-level
// replacement for the web UI's earlier client-side "chain of separate
// sessions" hack: a plain content reply pauses the session
// (store.SessionStatusAwaitingInput) instead of terminating it, and
// POST /v1/runs/{id}/steer transparently resumes it in place
// (internal/surfaces/rest/runctl.go's handleSteerRun) — same session_id,
// same audit trail, same cost ceiling, same Rule-of-Two taint state across
// every turn. This file proves the whole stack: the kernel's own
// pause/resume mechanics (isolated, driving kernel.Kernel directly — the
// same style permission_chain_test.go already uses), internal/runctl's
// ResumeConversation guard, and a full multi-turn REST round trip.
package integration

import (
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

	"github.com/truongpx396/nexus-agent-demo/internal/authn"
	"github.com/truongpx396/nexus-agent-demo/internal/crypto"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/internal/runctl"
	"github.com/truongpx396/nexus-agent-demo/internal/store"
	"github.com/truongpx396/nexus-agent-demo/internal/surfaces/rest"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// contentOnlyScript is a scripted provider turn with no tool call — the
// classification (kernel/turns.go's ClassificationContent) a conversational
// run reacts to by pausing rather than terminating.
func contentOnlyScript(text string) fake.Script {
	return fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "content", Text: text},
		{Kind: "done", Done: "stop"},
	}}
}

// --- store: GetSessionByKey, the primitive internal/surfaces/telegram
// (and zalo, email) use for deterministic per-peer session continuity ---

func TestStore_GetSessionByKey_NotFoundIsOkFalseNotError(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	var found bool
	err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		_, found, derr = store.GetSessionByKey(ctx, tx, tenantID, "telegram:no-such-chat")
		return derr
	})
	if err != nil {
		t.Fatalf("GetSessionByKey: %v", err)
	}
	if found {
		t.Fatal("expected found=false for a key with no session")
	}
}

func TestStore_GetSessionByKey_ReturnsMostRecentWhenKeyIsShared(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	// newTestSession creates one session with its OWN random SessionKey
	// (sessionID.String()); this test needs two sessions sharing the SAME
	// key, exactly like a peer's second conversation reusing the first
	// one's key after it went terminal — insert directly.
	tenantID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	const sharedKey = "telegram:12345"
	older := uuid.New()
	newer := uuid.New()
	err := st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, id := range []uuid.UUID{older, newer} {
			if err := store.CreateSession(ctx, tx, store.Session{
				SessionID: id, SessionKey: sharedKey, TenantID: tenantID,
				SurfaceID: "telegram", UserID: uuid.New(), AgentVersion: 1,
				HarnessDigest: []byte("test-digest"), DataLabel: "internal", RouteModelID: "test-model",
				AutonomyLevel: "supervised", Conversational: true,
			}); err != nil {
				return err
			}
			// created_at has whole-second precision risk in a fast test —
			// force ordering explicitly rather than relying on wall-clock
			// separation between the two inserts above.
			if _, err := tx.Exec(ctx, `UPDATE sessions SET created_at = now() WHERE session_id = $1`, id); err != nil {
				return err
			}
		}
		// newer must sort after older even at second-level timestamp
		// resolution.
		_, err := tx.Exec(ctx, `UPDATE sessions SET created_at = created_at + interval '1 second' WHERE session_id = $1`, newer)
		return err
	})
	if err != nil {
		t.Fatalf("create sessions: %v", err)
	}

	var got store.Session
	var found bool
	err = st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		got, found, derr = store.GetSessionByKey(ctx, tx, tenantID, sharedKey)
		return derr
	})
	if err != nil {
		t.Fatalf("GetSessionByKey: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if got.SessionID != newer {
		t.Fatalf("GetSessionByKey returned session %s, want the most recent one %s", got.SessionID, newer)
	}
}

// --- kernel-level: the pause/resume mechanics themselves ---

func TestKernel_ConversationalRunPausesInsteadOfTerminating(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID, sessionID, seal := newTestSession(t, ctx, st, "supervised")

	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fake.New(contentOnlyScript("Hi there!"))}),
		Tools:    kernel.NotImplementedToolExecutor{},
		Budget:   kernel.NoopBudgetGate{},
		Store:    st,
	}
	runState := &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal}
	cfg := kernel.RunConfig{
		System: "test", ModelID: "test-model", MaxTurns: 5, Input: "hello",
		AutonomyLevel: "supervised", Conversational: true,
	}

	var events []store.Event
	for ev, err := range k.Run(ctx, runState, cfg) {
		if err != nil {
			t.Fatalf("Run() yielded error: %v", err)
		}
		events = append(events, ev)
	}

	if len(events) == 0 || events[len(events)-1].Type != store.EventAwaitingInput {
		t.Fatalf("last event type = %v, want awaiting_input", events[len(events)-1].Type)
	}
	for _, ev := range events {
		if ev.Type == store.EventTerminal {
			t.Fatal("a conversational pause must never append a terminal event")
		}
	}

	sess, err := getSessionDirect(ctx, st, tenantID, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Status != store.SessionStatusAwaitingInput {
		t.Fatalf("session status = %q, want %q", sess.Status, store.SessionStatusAwaitingInput)
	}
	if sess.TerminalReason != nil {
		t.Fatalf("terminal_reason = %v, want nil (a conversational pause is not a terminal state)", *sess.TerminalReason)
	}
}

// TestKernel_NonConversationalRunStillTerminatesOnPlainReply is the
// regression guard the whole feature leans on: RunConfig.Conversational
// defaults to false, and false must reproduce today's behavior byte for
// byte — every pre-existing caller (REST default, CLI, Telegram, Zalo,
// email, cron) never sets it.
func TestKernel_NonConversationalRunStillTerminatesOnPlainReply(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID, sessionID, seal := newTestSession(t, ctx, st, "supervised")

	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fake.New(contentOnlyScript("Hi there!"))}),
		Tools:    kernel.NotImplementedToolExecutor{},
		Budget:   kernel.NoopBudgetGate{},
		Store:    st,
	}
	runState := &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal}
	cfg := kernel.RunConfig{System: "test", ModelID: "test-model", MaxTurns: 5, Input: "hello", AutonomyLevel: "supervised"} // Conversational left at its zero value

	var events []store.Event
	for ev, err := range k.Run(ctx, runState, cfg) {
		if err != nil {
			t.Fatalf("Run() yielded error: %v", err)
		}
		events = append(events, ev)
	}

	if len(events) == 0 || events[len(events)-1].Type != store.EventTerminal {
		t.Fatalf("last event type = %v, want terminal", events[len(events)-1].Type)
	}
	sess, err := getSessionDirect(ctx, st, tenantID, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Status != store.SessionStatusCompleted {
		t.Fatalf("session status = %q, want %q", sess.Status, store.SessionStatusCompleted)
	}
}

func TestKernel_ResumeConversationContinuesAndPreservesTranscript(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID, sessionID, seal := newTestSession(t, ctx, st, "supervised")

	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fake.New(
			contentOnlyScript("Hi there!"),
			contentOnlyScript("Sure, here's more detail."),
		)}),
		Tools:  kernel.NotImplementedToolExecutor{},
		Budget: kernel.NoopBudgetGate{},
		Store:  st,
	}
	runState := &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal}
	cfg := kernel.RunConfig{
		System: "test", ModelID: "test-model", MaxTurns: 5, Input: "hello",
		AutonomyLevel: "supervised", Conversational: true,
	}

	for ev, err := range k.Run(ctx, runState, cfg) {
		if err != nil {
			t.Fatalf("Run() yielded error: %v", err)
		}
		_ = ev
	}
	if len(runState.Transcript) == 0 {
		t.Fatal("expected the first turn to have populated Transcript")
	}
	firstTurnTranscriptLen := len(runState.Transcript)

	var events []store.Event
	for ev, err := range k.ResumeConversation(ctx, runState, cfg, "tell me more") {
		if err != nil {
			t.Fatalf("ResumeConversation() yielded error: %v", err)
		}
		events = append(events, ev)
	}

	if len(runState.Transcript) <= firstTurnTranscriptLen {
		t.Fatalf("Transcript did not grow across the resumed turn: had %d, now %d", firstTurnTranscriptLen, len(runState.Transcript))
	}
	if len(events) == 0 || events[len(events)-1].Type != store.EventAwaitingInput {
		t.Fatalf("last event type = %v, want awaiting_input (the resumed turn is ALSO a plain reply)", events[len(events)-1].Type)
	}

	full, err := listEventsDirect(ctx, st, tenantID, sessionID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var userMessages, awaitingInputs int
	for _, e := range full {
		switch e.Type { //nolint:exhaustive // only user_message/awaiting_input matter to this assertion
		case store.EventUserMessage:
			userMessages++
		case store.EventAwaitingInput:
			awaitingInputs++
		}
	}
	if userMessages != 2 {
		t.Fatalf("durable log has %d user_message events, want 2 (opening turn + resumed turn)", userMessages)
	}
	if awaitingInputs != 2 {
		t.Fatalf("durable log has %d awaiting_input events, want 2 (one pause per turn)", awaitingInputs)
	}
}

// --- internal/runctl: the ResumeConversation guard ---

func TestRunCtl_ResumeConversation_RefusesWhenNotAwaitingInput(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	// loadRunState (this method's own first step) needs a session with at
	// least one real, decryptable event to rehydrate from -- a completely
	// fresh, event-free session fails there before the status check this
	// test wants to exercise ever runs (phase6Rig.seedOneEvent's own doc
	// comment names this same requirement for every other out-of-band
	// runctl operation). So: run one NON-conversational turn to a real
	// terminal status first -- valid, decryptable history under a KeyStore
	// this test keeps, and a status that is definitely not awaiting_input.
	tenantID := uuid.New()
	sessionID := uuid.New()
	userID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("generate KEK: %v", err)
	}
	keys := crypto.NewKeyStore(kek)
	var dek crypto.DEK
	err = st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		dek, derr = keys.NewDEK(ctx, tx, tenantID)
		if derr != nil {
			return derr
		}
		return store.CreateSession(ctx, tx, store.Session{
			SessionID: sessionID, SessionKey: sessionID.String(), TenantID: tenantID,
			SurfaceID: "test", UserID: userID, AgentVersion: 1,
			HarnessDigest: []byte("test-digest"), DataLabel: "internal", RouteModelID: "test-model",
			AutonomyLevel: "supervised",
		})
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	seal := func(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", err
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}

	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fake.New(contentOnlyScript("done"))}),
		Tools:    kernel.NotImplementedToolExecutor{},
		Budget:   kernel.NoopBudgetGate{},
		Store:    st,
	}
	runState := &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal}
	cfg := kernel.RunConfig{System: "test", ModelID: "test-model", MaxTurns: 5, Input: "hello", AutonomyLevel: "supervised"} // Conversational left false
	for ev, err := range k.Run(ctx, runState, cfg) {
		if err != nil {
			t.Fatalf("Run(): %v", err)
		}
		_ = ev
	}
	if sess, err := getSessionDirect(ctx, st, tenantID, sessionID); err != nil {
		t.Fatalf("get session: %v", err)
	} else if sess.Status == store.SessionStatusAwaitingInput {
		t.Fatalf("test setup bug: session is already awaiting_input (status=%q)", sess.Status)
	}

	ctl := &runctl.Control{Store: st, Keys: keys, Kernel: k, System: "test", MaxTurns: 5}

	if _, err := ctl.ResumeConversation(ctx, tenantID, sessionID, "hello"); err == nil {
		t.Fatal("expected ResumeConversation to refuse a session that isn't awaiting_input")
	} else if !strings.Contains(err.Error(), "not awaiting_input") {
		t.Fatalf("error = %q, want it to mention not awaiting_input", err.Error())
	}
}

// TestRunCtl_ResumeConversation_Succeeds proves the DURABLE half of
// continuity end to end at the runctl layer: unlike the kernel-level tests
// above (which reuse the same in-memory *kernel.RunState across both
// calls), this drives ResumeConversation from a genuinely FRESH
// runctl.Control — it must rehydrate RunState from the durable log itself
// (internal/runctl/rehydrate.go) before it can continue, exactly the path
// a real process restart between turns would take.
func TestRunCtl_ResumeConversation_Succeeds(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	tenantID := uuid.New()
	sessionID := uuid.New()
	userID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("generate KEK: %v", err)
	}
	keys := crypto.NewKeyStore(kek)

	var dek crypto.DEK
	err = st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		dek, derr = keys.NewDEK(ctx, tx, tenantID)
		if derr != nil {
			return derr
		}
		return store.CreateSession(ctx, tx, store.Session{
			SessionID: sessionID, SessionKey: sessionID.String(), TenantID: tenantID,
			SurfaceID: "test", UserID: userID, AgentVersion: 1,
			HarnessDigest: []byte("test-digest"), DataLabel: "internal", RouteModelID: "test-model",
			AutonomyLevel: "supervised", Conversational: true,
		})
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	seal := func(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", err
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}

	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fake.New(
			contentOnlyScript("first reply"),
			contentOnlyScript("second reply"),
		)}),
		Tools:  kernel.NotImplementedToolExecutor{},
		Budget: kernel.NoopBudgetGate{},
		Store:  st,
	}
	runState := &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal}
	cfg := kernel.RunConfig{
		System: "test", ModelID: "test-model", MaxTurns: 5, Input: "hello",
		AutonomyLevel: "supervised", Conversational: true,
	}
	for ev, err := range k.Run(ctx, runState, cfg) {
		if err != nil {
			t.Fatalf("Run(): %v", err)
		}
		_ = ev
	}

	// A fresh Control -- no in-memory RunState carried over from the Run()
	// call above -- must rehydrate this session from the durable log.
	ctl := &runctl.Control{Store: st, Keys: keys, Kernel: k, System: "test", MaxTurns: 5}
	events, err := ctl.ResumeConversation(ctx, tenantID, sessionID, "tell me more")
	if err != nil {
		t.Fatalf("ResumeConversation refused a genuinely awaiting_input session: %v", err)
	}
	var last store.Event
	for ev, err := range events {
		if err != nil {
			t.Fatalf("ResumeConversation() yielded error: %v", err)
		}
		last = ev
	}
	if last.Type != store.EventAwaitingInput {
		t.Fatalf("last event type = %v, want awaiting_input", last.Type)
	}

	sess, err := getSessionDirect(ctx, st, tenantID, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Status != store.SessionStatusAwaitingInput {
		t.Fatalf("session status = %q, want %q", sess.Status, store.SessionStatusAwaitingInput)
	}
}

// TestRunCtl_EndIdleConversation is cmd/nexusd's idle-conversation sweep's
// own backstop (startIdleConversationSweepLoop/sweepIdleConversationsAllTenants),
// exercised directly against its core method rather than through the
// wall-clock ticker: a session sitting in awaiting_input gets a real
// terminal event and a real terminal status, distinct from an explicit
// human cancel (kernel.ReasonIdleTimeout vs kernel.ReasonAborted) — and a
// second call, after the session is already terminal, is a no-op rather
// than an error (mirrors Cancel's own already-terminal no-op convention).
func TestRunCtl_EndIdleConversation(t *testing.T) {
	pool, cleanup := setupPostgresAndPgBouncer(t)
	defer cleanup()
	ctx := context.Background()
	st := store.New(pool)

	// EndIdleConversation appends a real EventTerminal (internal/runctl/
	// append.go's own appendEvent), which needs a real DEK to seal
	// under -- build the session inline (rather than newTestSession, whose
	// KeyStore is private to that helper) so this test's own Control.Keys
	// is the SAME instance that minted the session's DEK.
	tenantID := uuid.New()
	sessionID := uuid.New()
	userID := uuid.New()
	if err := insertTenant(ctx, st, tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	kek, err := crypto.GenerateKEK()
	if err != nil {
		t.Fatalf("generate KEK: %v", err)
	}
	keys := crypto.NewKeyStore(kek)
	var dek crypto.DEK
	err = st.InTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		dek, derr = keys.NewDEK(ctx, tx, tenantID)
		if derr != nil {
			return derr
		}
		return store.CreateSession(ctx, tx, store.Session{
			SessionID: sessionID, SessionKey: sessionID.String(), TenantID: tenantID,
			SurfaceID: "test", UserID: userID, AgentVersion: 1,
			HarnessDigest: []byte("test-digest"), DataLabel: "internal", RouteModelID: "test-model",
			AutonomyLevel: "supervised", Conversational: true,
		})
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	seal := func(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
		sealed, err = crypto.Seal(dek, plaintext, tenantID.String(), sessionID.String())
		if err != nil {
			return nil, nil, "", err
		}
		return sealed, crypto.Digest(plaintext), dek.KeyID, nil
	}

	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fake.New(contentOnlyScript("hi"))}),
		Tools:    kernel.NotImplementedToolExecutor{},
		Budget:   kernel.NoopBudgetGate{},
		Store:    st,
	}
	runState := &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal}
	cfg := kernel.RunConfig{
		System: "test", ModelID: "test-model", MaxTurns: 5, Input: "hello",
		AutonomyLevel: "supervised", Conversational: true,
	}
	for ev, err := range k.Run(ctx, runState, cfg) {
		if err != nil {
			t.Fatalf("Run(): %v", err)
		}
		_ = ev
	}

	ctl := &runctl.Control{Store: st, Keys: keys, Kernel: k, System: "test", MaxTurns: 5}

	if err := ctl.EndIdleConversation(ctx, tenantID, sessionID); err != nil {
		t.Fatalf("EndIdleConversation: %v", err)
	}
	sess, err := getSessionDirect(ctx, st, tenantID, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Status != store.SessionStatusFailed {
		t.Fatalf("session status = %q, want %q", sess.Status, store.SessionStatusFailed)
	}
	if sess.TerminalReason == nil || *sess.TerminalReason != "idle_timeout" {
		t.Fatalf("terminal_reason = %v, want %q", sess.TerminalReason, "idle_timeout")
	}

	full, err := listEventsDirect(ctx, st, tenantID, sessionID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var terminals int
	for _, e := range full {
		if e.Type == store.EventTerminal {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("durable log has %d terminal events, want exactly 1", terminals)
	}

	// A second call, now that the session is terminal, is a no-op.
	if err := ctl.EndIdleConversation(ctx, tenantID, sessionID); err != nil {
		t.Fatalf("EndIdleConversation on an already-terminal session returned an error, want a silent no-op: %v", err)
	}
	full2, err := listEventsDirect(ctx, st, tenantID, sessionID)
	if err != nil {
		t.Fatalf("list events (second call): %v", err)
	}
	if len(full2) != len(full) {
		t.Fatalf("second EndIdleConversation call appended events (had %d, now %d), want no-op", len(full), len(full2))
	}
}

// --- full REST round trip ---

// testRunCtlPort implements rest.RunCtlPort over a real runctl.Control --
// mirrors cmd/nexusd/ports.go's nexusdRunCtlPort (package main, can't be
// imported from a test binary), the same duplication rationale
// testRunStarter's own doc comment gives for testRunStarter vs
// kernelRunStarter.
type testRunCtlPort struct {
	ctl *runctl.Control
}

func (p *testRunCtlPort) Cancel(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) error {
	return p.ctl.Cancel(ctx, tenantID, sessionID, reason)
}

func (p *testRunCtlPort) Steer(ctx context.Context, tenantID, sessionID uuid.UUID, input string) error {
	_, err := p.ctl.Steer(ctx, tenantID, sessionID, input)
	return err
}

func (p *testRunCtlPort) TightenAutonomy(ctx context.Context, tenantID, sessionID uuid.UUID, target string) error {
	return p.ctl.TightenAutonomy(ctx, tenantID, sessionID, target)
}

func (p *testRunCtlPort) Fork(context.Context, uuid.UUID, uuid.UUID, int64, string) (rest.ForkView, error) {
	return rest.ForkView{}, fmt.Errorf("test control plane: Fork not wired")
}

func (p *testRunCtlPort) ResumeConversation(ctx context.Context, tenantID, sessionID uuid.UUID, input string) (<-chan rest.RunEvent, error) {
	events, err := p.ctl.ResumeConversation(ctx, tenantID, sessionID, input)
	if err != nil {
		return nil, err
	}
	ch := make(chan rest.RunEvent, 8)
	go func() {
		defer close(ch)
		for ev, err := range events {
			ch <- rest.RunEvent{Event: ev, Err: err}
			if err != nil {
				return
			}
		}
	}()
	return ch, nil
}

func getRunStatus(t *testing.T, client *http.Client, baseURL, token, runID string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/runs/"+runID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/runs/%s: %v", runID, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body
	var got struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	return got.Status
}

// TestRESTConversationalSession_MultiTurnSameSessionID is the plan's own
// headline acceptance test: POST /v1/runs (conversational:true) reaches
// awaiting_input (not completed); three rounds of POST .../steer all
// resume the SAME session_id (never a new one); the durable log shows
// three user turns, three replies, three pauses, and — critically — zero
// terminal events until an explicit cancel finally ends it. This is what
// replaces the web UI's old client-side "start a new session per message"
// chaining (lib/threads.ts, now deleted) with genuine backend continuity.
func TestRESTConversationalSession_MultiTurnSameSessionID(t *testing.T) {
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

	fakeProvider := fake.New(
		contentOnlyScript("Hi! How can I help?"),
		contentOnlyScript("Sure, here's the answer to your follow-up."),
		contentOnlyScript("And here's a third."),
	)

	k := &kernel.Kernel{
		Provider: provider.Wrap([]provider.Provider{fakeProvider}),
		Tools:    kernel.NotImplementedToolExecutor{},
		Budget:   kernel.NoopBudgetGate{},
		Store:    st,
	}
	starter := &testRunStarter{kernel: k}
	ctl := &runctl.Control{Store: st, Keys: keyStore, Kernel: k, System: "test", MaxTurns: 10}

	signingKey, err := authn.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate test signing key: %v", err)
	}
	issuer := authn.NewDevIssuer(signingKey)
	verifier := authn.NewDevVerifier(authn.PublicKey(signingKey))
	token := mustIssueToken(t, issuer, tenantID, userID)

	srv := rest.NewServer(starter, st, keyStore, nil)
	srv.Verifier = verifier
	srv.ControlPlane = newTestControlPlane(st)
	srv.RunCtl = &testRunCtlPort{ctl: ctl}
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := httpSrv.Client()

	waitForStatus := func(runID, want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if got := getRunStatus(t, client, httpSrv.URL, token, runID); got == want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for session %s to reach status %q", runID, want)
	}

	// waitForAwaitingInputCount polls the DURABLE log rather than the
	// session's status field: status alone is racy here -- it reads
	// awaiting_input from turn N-1 for a moment even after POST .../steer
	// for turn N has already returned 200 (the async goroutine
	// (handleSteerRun's own dispatch) hasn't necessarily run yet), so a
	// naive "wait for status==awaiting_input" can return before the NEW
	// turn ever started, letting a following steer race the one still in
	// flight. The awaiting_input event COUNT only ever increases by
	// exactly one per completed turn, so waiting for it to reach `want` is
	// unambiguous regardless of timing.
	waitForAwaitingInputCount := func(sessionID uuid.UUID, want int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			events, err := listEventsDirect(ctx, st, tenantID, sessionID)
			if err != nil {
				t.Fatalf("list events: %v", err)
			}
			count := 0
			for _, e := range events {
				if e.Type == store.EventAwaitingInput {
					count++
				}
			}
			if count >= want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %d awaiting_input events in session %s", want, sessionID)
	}

	// Turn 1.
	createReq, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/runs", strings.NewReader(`{"input":"hello","conversational":true}`))
	createReq.Header.Set("Authorization", "Bearer "+token)
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
	sessionID := uuid.MustParse(created.RunID)

	waitForAwaitingInputCount(sessionID, 1)

	// Turn 2 and 3: POST .../steer -- must transparently resume the SAME
	// session (internal/surfaces/rest/runctl.go's handleSteerRun, routed
	// through ResumeConversation because the session is awaiting_input).
	// waitForAwaitingInputCount (not waitForStatus) between each: it must
	// block until THIS turn's own pause is durably recorded before the
	// next steer fires, or the next one can race a resume that's still in
	// flight (see that helper's own doc comment).
	for i, msg := range []string{"tell me more", "and one more thing"} {
		steerReq, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/runs/"+created.RunID+"/steer", strings.NewReader(`{"input":"`+msg+`"}`))
		steerReq.Header.Set("Authorization", "Bearer "+token)
		steerReq.Header.Set("content-type", "application/json")
		steerResp, err := client.Do(steerReq)
		if err != nil {
			t.Fatalf("POST steer %q: %v", msg, err)
		}
		_ = steerResp.Body.Close() // read-only response body
		if steerResp.StatusCode != http.StatusOK {
			t.Fatalf("POST steer %q status = %d, want 200", msg, steerResp.StatusCode)
		}
		waitForAwaitingInputCount(sessionID, i+2) // turn 1 already produced 1; this is the (i+2)th
	}
	waitForStatus(created.RunID, store.SessionStatusAwaitingInput)

	// The URL/session id never changed across all three turns (every
	// request above used the SAME created.RunID) -- now verify the durable
	// log agrees: one session, three user turns, three replies, three
	// pauses, never a terminal event.
	events, err := listEventsDirect(ctx, st, tenantID, sessionID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var userMessages, contents, awaitingInputs, terminals int
	for _, e := range events {
		switch e.Type { //nolint:exhaustive // only these four types matter to this assertion
		case store.EventUserMessage:
			userMessages++
		case store.EventContent:
			contents++
		case store.EventAwaitingInput:
			awaitingInputs++
		case store.EventTerminal:
			terminals++
		}
	}
	if userMessages != 3 {
		t.Fatalf("durable log has %d user_message events, want 3", userMessages)
	}
	if contents != 3 {
		t.Fatalf("durable log has %d content events, want 3", contents)
	}
	if awaitingInputs != 3 {
		t.Fatalf("durable log has %d awaiting_input events, want 3", awaitingInputs)
	}
	if terminals != 0 {
		t.Fatalf("durable log has %d terminal events, want 0 (a conversational session still being talked to must never terminate)", terminals)
	}

	// Cancel proves the session CAN still be ended explicitly, including
	// from awaiting_input (internal/runctl.Cancel's own "any non-terminal
	// status" contract).
	cancelReq, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/runs/"+created.RunID+"/cancel", strings.NewReader(`{"reason":"test done"}`))
	cancelReq.Header.Set("Authorization", "Bearer "+token)
	cancelReq.Header.Set("content-type", "application/json")
	cancelResp, err := client.Do(cancelReq)
	if err != nil {
		t.Fatalf("POST cancel: %v", err)
	}
	_ = cancelResp.Body.Close() // read-only response body
	if cancelResp.StatusCode != http.StatusOK {
		t.Fatalf("POST cancel status = %d, want 200", cancelResp.StatusCode)
	}
	waitForStatus(created.RunID, store.SessionStatusFailed)
}
