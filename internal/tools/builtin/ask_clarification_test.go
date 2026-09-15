package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

func TestAskClarification_Taint(t *testing.T) {
	taint := AskClarification{}.Taint()
	if taint.ReturnsUntrusted || taint.ReadsPrivateData || taint.MutatesExternal {
		t.Errorf("expected every Taint field false (a human's direct answer is trusted, not retrieved/private/external), got %+v", taint)
	}
}

func TestAskClarification_ValidateInput(t *testing.T) {
	a := AskClarification{}
	if err := a.ValidateInput(context.Background(), json.RawMessage(`{"question":"which region?"}`), tools.RunContext{}); err != nil {
		t.Errorf("expected a valid question to pass, got %v", err)
	}
	if err := a.ValidateInput(context.Background(), json.RawMessage(`{}`), tools.RunContext{}); err == nil {
		t.Error("expected a missing question to be rejected")
	}
}

func TestAskClarification_CheckPermissions_AlwaysAsks(t *testing.T) {
	a := AskClarification{}
	result := a.CheckPermissions(context.Background(), json.RawMessage(`{"question":"which region?"}`), tools.RunContext{})
	if result.Decision != "ask" {
		t.Errorf("expected Decision=ask unconditionally, got %q", result.Decision)
	}
}

func TestAskClarification_Call_EchoesResumedInput(t *testing.T) {
	a := AskClarification{}
	res, err := a.Call(context.Background(), json.RawMessage(`{"answer":"EU"}`), tools.RunContext{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %s", res.Reason)
	}
	var out struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.Answer != "EU" {
		t.Errorf("expected Call to echo the resumed input verbatim, got %+v", out)
	}
}
