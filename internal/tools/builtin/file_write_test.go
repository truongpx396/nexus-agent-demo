package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

func TestFileWrite_WritesFileCreatingParentDirs(t *testing.T) {
	dir := t.TempDir()
	rc := tools.RunContext{WorkspaceDir: dir}
	var f FileWrite

	in := json.RawMessage(`{"path":"nested/dir/out.txt","content":"hello"}`)
	if err := f.ValidateInput(context.Background(), in, rc); err != nil {
		t.Fatalf("ValidateInput: %v", err)
	}
	result, err := f.Call(context.Background(), in, rc)
	if err != nil || result.IsError {
		t.Fatalf("Call() = %+v, %v", result, err)
	}

	content, err := os.ReadFile(filepath.Join(dir, "nested", "dir", "out.txt")) //nolint:gosec // dir is t.TempDir(); the path is test-constructed, not external input
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(content) != "hello" {
		t.Fatalf("content = %q, want %q", content, "hello")
	}
}

func TestFileWrite_Taint(t *testing.T) {
	var f FileWrite
	taint := f.Taint()
	if taint.ReturnsUntrusted || taint.ReadsPrivateData || !taint.MutatesExternal {
		t.Fatalf("Taint = %+v, want {false false true}", taint)
	}
	if f.Descriptor().EffectClass != tools.EffectClassMutating {
		t.Fatalf("EffectClass = %v, want mutating", f.Descriptor().EffectClass)
	}
	if f.IsConcurrencySafe(nil) {
		t.Fatal("IsConcurrencySafe() = true, want false (writes must serialize)")
	}
}
