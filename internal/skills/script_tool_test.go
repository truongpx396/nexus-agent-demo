package skills

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

func TestScriptTool_RunsAndReturnsOutput(t *testing.T) {
	s := ScriptTool{SkillID: "triage-report", Content: []byte("echo hello-from-script")}
	result, err := s.Call(context.Background(), nil, tools.RunContext{})
	if err != nil {
		t.Fatalf("Call error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Call result is an error: %s", result.Reason)
	}
	var decoded struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if decoded.Output != "hello-from-script\n" {
		t.Errorf("Output = %q, want %q", decoded.Output, "hello-from-script\n")
	}
}

func TestScriptTool_ID_UsesSkillNamespace(t *testing.T) {
	s := ScriptTool{SkillID: "triage-report"}
	ref := s.ID()
	if ref.Namespace != "skill" || ref.Name != "triage-report" {
		t.Errorf("ID() = %+v, want namespace=skill name=triage-report", ref)
	}
}

func TestScriptTool_NonZeroExitIsAnErrorResult(t *testing.T) {
	s := ScriptTool{SkillID: "s", Content: []byte("exit 1")}
	result, err := s.Call(context.Background(), nil, tools.RunContext{})
	if err != nil {
		t.Fatalf("Call error = %v", err)
	}
	if !result.IsError {
		t.Fatal("Call result is not an error for a script that exited 1")
	}
}
