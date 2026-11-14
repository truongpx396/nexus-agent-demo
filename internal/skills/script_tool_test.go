package skills

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/truongpx396/nexus-agent/internal/tools"
)

// fakeSandbox records what it was asked to run and answers with a fixed result.
type fakeSandbox struct {
	ran      []string
	output   string
	exitCode int
	breach   string
	err      error
}

func (f *fakeSandbox) Exec(_ context.Context, cmd string) (string, int, string, error) {
	f.ran = append(f.ran, cmd)
	return f.output, f.exitCode, f.breach, f.err
}

func callWith(t *testing.T, s ScriptTool, sb tools.SandboxExec) tools.Result {
	t.Helper()
	result, err := s.Call(context.Background(), nil, tools.RunContext{Sandbox: sb})
	if err != nil {
		t.Fatalf("Call error = %v", err)
	}
	return result
}

func TestScriptTool_RunsInTheSandboxAndReturnsOutput(t *testing.T) {
	sb := &fakeSandbox{output: "hello-from-script\n"}
	result := callWith(t, ScriptTool{SkillID: "triage-report", Content: []byte("echo hello-from-script")}, sb)
	if result.IsError {
		t.Fatalf("Call result is an error: %s", result.Reason)
	}
	if len(sb.ran) != 1 || sb.ran[0] != "echo hello-from-script" {
		t.Errorf("sandbox ran %q, want exactly the bundle's script", sb.ran)
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

// With no sandbox the script must not run at all: the refusal is a result the
// model sees, not a local process the operator did not ask for.
func TestScriptTool_RefusesToRunWithoutASandbox(t *testing.T) {
	result := callWith(t, ScriptTool{SkillID: "s", Content: []byte("touch /tmp/should-never-exist")}, nil)
	if !result.IsError || result.Reason != ReasonNoSandbox {
		t.Fatalf("result = %+v, want an error with reason %q", result, ReasonNoSandbox)
	}
	if !strings.Contains(result.Reason, "NEXUS_SANDBOX") {
		t.Errorf("reason %q should tell the operator what to set", result.Reason)
	}
	if len(result.Output) != 0 {
		t.Errorf("a refused call produced output %q", result.Output)
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
	result := callWith(t, ScriptTool{SkillID: "s", Content: []byte("exit 1")}, &fakeSandbox{exitCode: 1})
	if !result.IsError || result.Reason != "exit status 1" {
		t.Fatalf("result = %+v, want an error result for exit status 1", result)
	}
}

func TestScriptTool_SandboxBreachIsATypedErrorResult(t *testing.T) {
	result := callWith(t, ScriptTool{SkillID: "s", Content: []byte("x")}, &fakeSandbox{breach: "timeout"})
	if !result.IsError || result.Reason != "sandbox_breach: timeout" {
		t.Fatalf("result = %+v, want sandbox_breach: timeout", result)
	}
}

func TestScriptTool_SandboxFailureIsAnErrorResultNotAGoError(t *testing.T) {
	result := callWith(t, ScriptTool{SkillID: "s", Content: []byte("x")}, &fakeSandbox{err: errors.New("daemon down")})
	if !result.IsError || result.Reason != "daemon down" {
		t.Fatalf("result = %+v, want the sandbox's own error as the reason", result)
	}
}
