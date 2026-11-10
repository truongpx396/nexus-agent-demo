package kernel_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/truongpx396/nexus-agent-demo/internal/eventlog"
	"github.com/truongpx396/nexus-agent-demo/kernel"
)

// A resolution the kernel does not recognise must stop the run. Reading it as
// "no result" would append an empty, successful tool_result and let the loop
// carry on past an execution nobody approved.

func TestResume_AnUnknownApprovalDecisionStopsTheRunAndRecordsNoResult(t *testing.T) {
	r := newRig(toolFunc(func(kernel.ToolUseRequest) kernel.ToolResult {
		t.Error("the tool ran for a decision the kernel does not recognise")
		return kernel.ToolResult{}
	}), finalTurn)
	cfg := kernel.RunConfig{System: "test", ModelID: "fake", MaxTurns: 5, AutonomyLevel: "supervised"}

	var gotErr error
	for _, err := range r.k.Resume(context.Background(), r.st, cfg, kernel.PendingResolution{
		ToolID: "platform/echo@v1", ToolUseEventID: uuid.New(), Decision: kernel.ApprovalDecisionKind("maybe"),
	}) {
		if err != nil {
			gotErr = err
			break
		}
	}

	if gotErr == nil || !strings.Contains(gotErr.Error(), "unknown approval decision") {
		t.Fatalf("Resume with an unknown decision = %v, want an unknown-decision error", gotErr)
	}
	if n := len(ofType(r.log.Events(r.sessionID), eventlog.EventToolResult)); n != 0 {
		t.Fatalf("an unknown decision recorded %d tool_result events, want none", n)
	}
}

func TestResumeDelegation_AnUnknownOutcomeStopsTheRunAndRecordsNoResult(t *testing.T) {
	r := newRig(toolFunc(func(kernel.ToolUseRequest) kernel.ToolResult { return kernel.ToolResult{} }), finalTurn)
	cfg := kernel.RunConfig{System: "test", ModelID: "fake", MaxTurns: 5, AutonomyLevel: "supervised"}

	var gotErr error
	for _, err := range r.k.ResumeDelegation(context.Background(), r.st, cfg, kernel.DelegationResolution{
		ToolID: "platform/delegate@v1", ToolUseEventID: uuid.New(), Outcome: kernel.DelegationOutcomeKind("lost"),
	}) {
		if err != nil {
			gotErr = err
			break
		}
	}

	if gotErr == nil || !strings.Contains(gotErr.Error(), "unknown outcome") {
		t.Fatalf("ResumeDelegation with an unknown outcome = %v, want an unknown-outcome error", gotErr)
	}
	if n := len(ofType(r.log.Events(r.sessionID), eventlog.EventToolResult)); n != 0 {
		t.Fatalf("an unknown outcome recorded %d tool_result events, want none", n)
	}
}
