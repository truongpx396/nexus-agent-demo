package builtin

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

func TestShell_RunsCommandInWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	dir := t.TempDir()
	s := Shell{}
	result, err := s.Call(context.Background(), json.RawMessage(`{"cmd":"pwd"}`), tools.RunContext{WorkspaceDir: dir})
	if err != nil || result.IsError {
		t.Fatalf("Call() = %+v, %v", result, err)
	}
	var decoded struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(decoded.Output, dir) {
		t.Fatalf("pwd output %q does not mention workspace dir %q", decoded.Output, dir)
	}
}

func TestShell_NonZeroExitIsReportedAsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	s := Shell{}
	result, err := s.Call(context.Background(), json.RawMessage(`{"cmd":"exit 3"}`), tools.RunContext{})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("Call() with a non-zero exit did not report IsError")
	}
}

func TestShell_TimeoutBoundsExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	s := Shell{Timeout: 20 * time.Millisecond}
	start := time.Now()
	result, err := s.Call(context.Background(), json.RawMessage(`{"cmd":"sleep 5"}`), tools.RunContext{})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("Call() did not report an error for a command that exceeded its timeout")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Call() took %v, want it bounded near the configured 20ms timeout", elapsed)
	}
}

func TestShell_TaintIsFailClosedDefault(t *testing.T) {
	var s Shell
	if s.Taint() != tools.DefaultTaint() {
		t.Fatalf("Taint() = %+v, want the fail-closed default", s.Taint())
	}
	if s.IsConcurrencySafe(nil) {
		t.Fatal("IsConcurrencySafe() = true, want false")
	}
}

// An unsandboxed shell used to inherit the server's whole environment: the model
// provider's API key, the database URLs and the key file paths, readable by any
// command the model wrote.
func TestShell_UnsandboxedCommandDoesNotInheritTheServersEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	t.Setenv("NEXUS_TEST_SECRET", "leak")
	t.Setenv("ANTHROPIC_API_KEY", "sk-leak")

	dir := t.TempDir()
	result, err := Shell{}.Call(context.Background(), json.RawMessage(`{"cmd":"env"}`), tools.RunContext{WorkspaceDir: dir})
	if err != nil || result.IsError {
		t.Fatalf("Call() = %+v, %v", result, err)
	}
	var decoded struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"leak", "NEXUS_TEST_SECRET", "ANTHROPIC_API_KEY"} {
		if strings.Contains(decoded.Output, leaked) {
			t.Errorf("the command's environment contains %q:\n%s", leaked, decoded.Output)
		}
	}
	for _, want := range []string{"PATH=", "HOME=" + dir, "LANG=C.UTF-8"} {
		if !strings.Contains(decoded.Output, want) {
			t.Errorf("the command's environment lacks %q:\n%s", want, decoded.Output)
		}
	}

	// A command still finds the usual tools on that PATH.
	ok, err := Shell{}.Call(context.Background(), json.RawMessage(`{"cmd":"ls / > /dev/null && echo fine"}`), tools.RunContext{})
	if err != nil || ok.IsError {
		t.Fatalf("a basic command failed with the reduced environment: %+v, %v", ok, err)
	}
}

func TestShellEnv_HomeFallsBackToTheTempDirectory(t *testing.T) {
	env := shellEnv("")
	if len(env) != 3 || env[1] != "HOME="+os.TempDir() {
		t.Fatalf("shellEnv(\"\") = %v, want HOME at the system temp directory", env)
	}
}
