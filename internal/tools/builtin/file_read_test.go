package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

func TestFileRead_ReadsWorkspaceFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi there"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	rc := tools.RunContext{WorkspaceDir: dir}

	var f FileRead
	in := json.RawMessage(`{"path":"hello.txt"}`)
	if err := f.ValidateInput(context.Background(), in, rc); err != nil {
		t.Fatalf("ValidateInput: %v", err)
	}
	result, err := f.Call(context.Background(), in, rc)
	if err != nil {
		t.Fatalf("Call error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Call result is an error: %s", result.Reason)
	}
	var decoded struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if decoded.Content != "hi there" {
		t.Fatalf("Content = %q, want %q", decoded.Content, "hi there")
	}
}

func TestFileRead_RefusesTraversalOutsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	rc := tools.RunContext{WorkspaceDir: dir}
	var f FileRead
	result, err := f.Call(context.Background(), json.RawMessage(`{"path":"../../etc/passwd"}`), rc)
	if err != nil {
		t.Fatalf("Call error = %v", err)
	}
	// resolveWorkspacePath maps this INTO the workspace (../../etc/passwd ->
	// <workspace>/etc/passwd), which then simply doesn't exist — the point
	// is that no error path here can ever read a file outside dir.
	if !result.IsError {
		t.Fatal("Call() succeeded reading a path that should not exist under the workspace")
	}
}

func TestFileRead_ValidateInputRequiresPath(t *testing.T) {
	var f FileRead
	if err := f.ValidateInput(context.Background(), json.RawMessage(`{}`), tools.RunContext{}); err == nil {
		t.Fatal("ValidateInput({}) = nil error, want an error")
	}
}

func TestFileRead_Descriptor(t *testing.T) {
	var f FileRead
	d := f.Descriptor()
	if d.EffectClass != tools.EffectClassReadOnly {
		t.Fatalf("EffectClass = %v, want read_only", d.EffectClass)
	}
	taint := f.Taint()
	if !taint.ReturnsUntrusted || taint.ReadsPrivateData || taint.MutatesExternal {
		t.Fatalf("Taint = %+v, want {true false false}", taint)
	}
}
