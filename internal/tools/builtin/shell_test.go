package builtin

import (
	"context"
	"encoding/json"
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
