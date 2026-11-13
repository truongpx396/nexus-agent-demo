package kernel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/internal/provider"
	"github.com/truongpx396/nexus-agent-demo/internal/provider/fake"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

func callTurn(id string) fake.Script {
	return fake.Script{Chunks: []fake.ChunkSpec{
		{Kind: "tool_use", ToolUseID: id, ToolName: "platform/echo@v1", Input: `{"msg":"hi"}`},
		{Kind: "done", Done: "stop"},
	}}
}

// condensingRig runs a session until it has condensed once: three tool turns of
// 600 bytes of output each pass 1,500 bytes at the start of the fourth turn, the
// condenser's own model call answers "SUMMARY", and the fourth turn finishes.
func condensingRig(t *testing.T) (*rig, *kernel.RunState) {
	t.Helper()
	return condensingRigWith(t, nil)
}

// condensingRigWith is condensingRig with a budget gate of the caller's (nil keeps the no-op one).
func condensingRigWith(t *testing.T, budget kernel.BudgetGate) (*rig, *kernel.RunState) {
	t.Helper()
	out := json.RawMessage(`"` + strings.Repeat("o", 600) + `"`)
	r := newRig(toolFunc(func(kernel.ToolUseRequest) kernel.ToolResult { return kernel.ToolResult{Output: out} }),
		callTurn("tu1"), callTurn("tu2"), callTurn("tu3"),
		fake.Script{Chunks: []fake.ChunkSpec{{Kind: "content", Text: "SUMMARY"}, {Kind: "done", Done: "stop"}}}, // the condenser
		finalTurn,
	)
	r.k.CondenseThresholdBytes = 1500
	if budget != nil {
		r.k.Budget = budget
	}
	cfg := kernel.RunConfig{System: "test", ModelID: "fake", CondenserModelID: "cheap", MaxTurns: 10, Input: "go", AutonomyLevel: "supervised"}
	for _, err := range r.k.Run(context.Background(), r.st, cfg) {
		if err != nil {
			t.Fatal(err)
		}
	}
	return r, r.st
}

// Defect 4: the condensed transcript lived only in memory, and every queued start,
// approval and follow-up rehydrates from the log, so the full history came back and
// was condensed again with another model call.
func TestRehydrate_ReplaysACondensationSoAResumedRunKeepsIt(t *testing.T) {
	r, live := condensingRig(t)
	history := r.log.Events(r.sessionID)

	conds := ofType(history, eventlog.EventCondensation)
	if len(conds) != 1 {
		t.Fatalf("the run wrote %d condensation events, want 1 (the fixture must condense exactly once)", len(conds))
	}
	var p struct {
		Summary         string `json:"summary"`
		CoveredMessages int    `json:"covered_messages"`
	}
	if err := json.Unmarshal(conds[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Summary != "SUMMARY" || p.CoveredMessages != 3 {
		t.Fatalf("condensation payload = %+v, want summary SUMMARY covering the first 3 messages (the user turn and the first call and result)", p)
	}

	rebuilt, _, err := kernel.Rehydrate(context.Background(), history, openPayload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rebuilt, live.Transcript) {
		t.Fatalf("Rehydrate does not reproduce the live transcript.\nlive:    %s\nrebuilt: %s", dump(live.Transcript), dump(rebuilt))
	}
	if err := provider.ValidateTranscript(rebuilt); err != nil {
		t.Fatalf("the condensed transcript is not valid: %v", err)
	}
	first := rebuilt[0]
	if first.Role != "user" || !strings.HasPrefix(first.PlainText(), "[summary of the earlier conversation]\nSUMMARY") {
		t.Fatalf("the transcript opens with %s %q, want the summary as a user turn", first.Role, first.PlainText())
	}
}

// A second resume of the same log starts from the condensed transcript, so it does
// not ask the condenser again.
func TestContinue_ASecondResumeDoesNotCondenseAgain(t *testing.T) {
	r, _ := condensingRig(t)
	history := r.log.Events(r.sessionID)

	rec := &streamRecorder{inner: fake.New(finalTurn)}
	r.k.Provider = rec
	transcript, ids, err := kernel.Rehydrate(context.Background(), history, openPayload)
	if err != nil {
		t.Fatal(err)
	}
	st := &kernel.RunState{TenantID: r.tenantID, SessionID: r.sessionID, Seal: seal, History: history, Transcript: transcript, ToolUseIDs: ids}
	cfg := kernel.RunConfig{System: "test", ModelID: "fake", CondenserModelID: "cheap", MaxTurns: 10, AutonomyLevel: "supervised"}
	for _, err := range r.k.Continue(context.Background(), st, cfg) {
		if err != nil {
			t.Fatal(err)
		}
	}

	if n := len(ofType(r.log.Events(r.sessionID), eventlog.EventCondensation)); n != 1 {
		t.Fatalf("the log has %d condensation events after a second resume, want still 1", n)
	}
	if len(rec.systems) != 1 {
		t.Fatalf("the resumed run called the provider %d times, want once (the turn, no condenser call)", len(rec.systems))
	}
}

func dump(msgs []provider.Message) string { return fmt.Sprintf("%+v", msgs) }
