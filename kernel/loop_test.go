package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/eventlog/inmem"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// These tests drive the real loop against the in-memory EventLog and the fake
// provider: no database, no Docker. They are the reason EventLog is a port —
// the loop's own guarantees (event order, paired results, what gets recorded
// on a suspension, how a failing log surfaces) can be pinned in milliseconds,
// leaving tests/integration to prove what only Postgres can (tenant isolation
// through PgBouncer, atomic audit receipts).

type toolFunc func(kernel.ToolUseRequest) kernel.ToolResult

func (f toolFunc) Execute(_ context.Context, req kernel.ToolUseRequest, _ kernel.ExecContext) kernel.ToolResult {
	return f(req)
}

func seal(plaintext []byte) (sealed, digest []byte, keyID string, err error) {
	return plaintext, []byte("digest"), "test-key", nil
}

var toolTurn = fake.Script{Chunks: []fake.ChunkSpec{
	{Kind: "tool_use", ToolUseID: "tu1", ToolName: "platform/echo@v1", Input: `{"msg":"hi"}`},
	{Kind: "done", Done: "stop"},
}}

var finalTurn = fake.Script{Chunks: []fake.ChunkSpec{
	{Kind: "content", Text: "all done"},
	{Kind: "done", Done: "stop"},
}}

type rig struct {
	k         *kernel.Kernel
	log       *inmem.Log
	st        *kernel.RunState
	tenantID  uuid.UUID
	sessionID uuid.UUID
}

func newRig(tools kernel.ToolExecutor, scripts ...fake.Script) *rig {
	log := inmem.New()
	return newRigWithLog(log, log, tools, scripts...)
}

// newRigWithLog lets a test wrap the in-memory log (to inject a fault) while
// still reading back from the underlying one.
func newRigWithLog(inner *inmem.Log, log kernel.EventLog, tools kernel.ToolExecutor, scripts ...fake.Script) *rig {
	tenantID, sessionID := uuid.New(), uuid.New()
	return &rig{
		k: &kernel.Kernel{
			Provider: provider.Wrap([]provider.Provider{fake.New(scripts...)}),
			Tools:    tools,
			Budget:   kernel.NoopBudgetGate{},
			Log:      log,
		},
		log:       inner,
		st:        &kernel.RunState{TenantID: tenantID, SessionID: sessionID, Seal: seal},
		tenantID:  tenantID,
		sessionID: sessionID,
	}
}

func (r *rig) run(t *testing.T) (events []eventlog.Event, err error) {
	t.Helper()
	cfg := kernel.RunConfig{System: "test", ModelID: "fake", MaxTurns: 5, Input: "go", AutonomyLevel: "supervised"}
	for ev, rerr := range r.k.Run(context.Background(), r.st, cfg) {
		if rerr != nil {
			return events, rerr
		}
		events = append(events, ev)
	}
	return events, nil
}

func ofType(events []eventlog.Event, typ eventlog.EventType) []eventlog.Event {
	var out []eventlog.Event
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestRun_AToolTurnThenAnAnswerIsRecordedInOrderWithItsResultPaired(t *testing.T) {
	r := newRig(
		toolFunc(func(kernel.ToolUseRequest) kernel.ToolResult {
			return kernel.ToolResult{Output: json.RawMessage(`{"echo":"hi"}`)}
		}),
		toolTurn, finalTurn,
	)

	events, err := r.run(t)
	if err != nil {
		t.Fatalf("Run yielded an error: %v", err)
	}

	logged := r.log.Events(r.sessionID)
	if len(logged) != len(events) {
		t.Fatalf("the loop yielded %d events but the log holds %d: a yielded event must already be durable", len(events), len(logged))
	}
	for i, e := range logged {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has Seq %d, want a gapless sequence from 1", i, e.Seq)
		}
		if e.EventID != events[i].EventID {
			t.Fatalf("event %d: yielded %s, logged %s", i, events[i].EventID, e.EventID)
		}
	}
	if last := logged[len(logged)-1]; last.Type != eventlog.EventTerminal {
		t.Fatalf("last event is %s, want terminal", last.Type)
	}

	// The paired-result invariant (constitution Principle II): every tool_use
	// has its tool_result, linked by PairRef, before the next model call.
	uses, results := ofType(logged, eventlog.EventToolUse), ofType(logged, eventlog.EventToolResult)
	if len(uses) != 1 || len(results) != 1 {
		t.Fatalf("got %d tool_use and %d tool_result events, want 1 and 1", len(uses), len(results))
	}
	if results[0].PairRef == nil || *results[0].PairRef != uses[0].EventID {
		t.Fatalf("tool_result.PairRef = %v, want the tool_use's EventID %s", results[0].PairRef, uses[0].EventID)
	}
	if results[0].Seq <= uses[0].Seq {
		t.Fatalf("tool_result (seq %d) does not follow its tool_use (seq %d)", results[0].Seq, uses[0].Seq)
	}

	status, ok := r.log.Status(r.sessionID)
	if !ok || status.Value != eventlog.SessionStatusCompleted {
		t.Fatalf("session status = %+v (set=%v), want completed", status, ok)
	}
	if status.TerminalReason == nil || *status.TerminalReason != string(kernel.ReasonCompleted) {
		t.Fatalf("terminal reason = %v, want %q", status.TerminalReason, kernel.ReasonCompleted)
	}
}

func TestRun_AnApprovalSuspendsTheRunAndRecordsItThroughThePort(t *testing.T) {
	r := newRig(
		toolFunc(func(kernel.ToolUseRequest) kernel.ToolResult {
			return kernel.ToolResult{
				IsError: true, AwaitingApproval: true, AskKind: "once",
				Reason: "needs a human", CanonicalDigest: []byte("canon"), EffectClass: "external_write",
			}
		}),
		toolTurn,
	)

	events, err := r.run(t)
	if err != nil {
		t.Fatalf("Run yielded an error: %v", err)
	}

	if len(ofType(events, eventlog.EventTerminal)) != 0 {
		t.Fatal("a suspended run is paused, not done: it must not append a terminal event")
	}
	status, _ := r.log.Status(r.sessionID)
	if status.Value != eventlog.SessionStatusSuspended {
		t.Fatalf("session status = %q, want suspended", status.Value)
	}

	got := r.log.Suspensions()
	if len(got) != 1 {
		t.Fatalf("recorded %d suspensions, want 1", len(got))
	}
	use := ofType(events, eventlog.EventToolUse)[0]
	requested := ofType(events, eventlog.EventApprovalRequested)[0]
	want := kernel.SuspendRequest{
		TenantID: r.tenantID, SessionID: r.sessionID,
		ToolUseEventID: use.EventID, ApprovalEventID: requested.EventID,
		ToolID: "platform/echo@v1", Input: json.RawMessage(`{"msg":"hi"}`),
		CanonicalDigest: []byte("canon"), AskKind: "once", EffectClass: "external_write",
	}
	if g := got[0]; g.TenantID != want.TenantID || g.SessionID != want.SessionID ||
		g.ToolUseEventID != want.ToolUseEventID || g.ApprovalEventID != want.ApprovalEventID ||
		g.ToolID != want.ToolID || string(g.Input) != string(want.Input) ||
		string(g.CanonicalDigest) != string(want.CanonicalDigest) || g.AskKind != want.AskKind || g.EffectClass != want.EffectClass {
		t.Fatalf("suspension = %+v, want %+v", g, want)
	}
}

func TestRun_ADelegationSuspendsTheRunAndBindsItThroughThePort(t *testing.T) {
	child := uuid.New()
	r := newRig(
		toolFunc(func(kernel.ToolUseRequest) kernel.ToolResult {
			return kernel.ToolResult{AwaitingDelegation: true, ChildSessionID: child}
		}),
		toolTurn,
	)

	events, err := r.run(t)
	if err != nil {
		t.Fatalf("Run yielded an error: %v", err)
	}

	if len(ofType(events, eventlog.EventTerminal)) != 0 {
		t.Fatal("a run waiting on a delegation is paused, not done: it must not append a terminal event")
	}
	got := r.log.Delegations()
	if len(got) != 1 {
		t.Fatalf("recorded %d delegations, want 1", len(got))
	}
	use := ofType(events, eventlog.EventToolUse)[0]
	requested := ofType(events, eventlog.EventDelegationRequested)[0]
	if g := got[0]; g.ChildSessionID != child || g.ToolUseEventID != use.EventID || g.DelegationEventID != requested.EventID || g.SessionID != r.sessionID {
		t.Fatalf("delegation = %+v, want child %s, tool_use %s, delegation event %s", g, child, use.EventID, requested.EventID)
	}
}

func TestRun_AFailingLogStopsTheRunAndNothingIsAppendedAfterIt(t *testing.T) {
	errDisk := errors.New("disk full")
	log := inmem.New()
	log.FailAppend = func(e eventlog.Event) error {
		if e.Type == eventlog.EventToolResult {
			return errDisk
		}
		return nil
	}
	r := newRigWithLog(log, log,
		toolFunc(func(kernel.ToolUseRequest) kernel.ToolResult {
			return kernel.ToolResult{Output: json.RawMessage(`{}`)}
		}),
		toolTurn, finalTurn,
	)

	_, err := r.run(t)
	if !errors.Is(err, errDisk) {
		t.Fatalf("Run error = %v, want it to wrap the log's error %v", err, errDisk)
	}

	logged := r.log.Events(r.sessionID)
	if n := len(ofType(logged, eventlog.EventTerminal)); n != 0 {
		t.Fatalf("log holds %d terminal events after an append failure; the run must stop, not carry on", n)
	}
	if last := logged[len(logged)-1]; last.Type != eventlog.EventToolUse {
		t.Fatalf("last durable event is %s, want the tool_use whose result could not be recorded", last.Type)
	}
}

// failingSuspensions makes only RecordSuspension fail, to prove the loop
// reports a durable-record failure rather than yielding a suspension that
// nothing can ever resolve.
type failingSuspensions struct {
	*inmem.Log
	err error
}

func (f failingSuspensions) RecordSuspension(context.Context, kernel.SuspendRequest) error {
	return f.err
}

func TestRun_AFailureToRecordTheSuspensionIsSurfaced(t *testing.T) {
	errDB := errors.New("approvals table unavailable")
	log := inmem.New()
	r := newRigWithLog(log, failingSuspensions{Log: log, err: errDB},
		toolFunc(func(kernel.ToolUseRequest) kernel.ToolResult {
			return kernel.ToolResult{IsError: true, AwaitingApproval: true, AskKind: "once"}
		}),
		toolTurn,
	)

	_, err := r.run(t)
	if !errors.Is(err, errDB) {
		t.Fatalf("Run error = %v, want it to wrap %v", err, errDB)
	}
	if !strings.Contains(err.Error(), "recording the suspension failed") {
		t.Fatalf("Run error %q does not say the suspension could not be recorded", err)
	}
}
