package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/truongpx396/nexus-agent-demo/internal/tools"
)

func TestFileSearch_MatchesGlobPattern(t *testing.T) {
	dir := t.TempDir()
	files := []string{"a.go", "b.go", "readme.md", filepath.Join("sub", "c.go")}
	for _, f := range files {
		full := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("setup mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatalf("setup write: %v", err)
		}
	}

	var s FileSearch
	result, err := s.Call(context.Background(), json.RawMessage(`{"pattern":"*.go"}`), tools.RunContext{WorkspaceDir: dir})
	if err != nil || result.IsError {
		t.Fatalf("Call() = %+v, %v", result, err)
	}
	var decoded struct {
		Matches []string `json:"matches"`
	}
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sort.Strings(decoded.Matches)
	// filepath.Match's "*.go" matches by base name for a.go/b.go/sub/c.go
	// (matched against the base name fallback) and by full relative path
	// for a.go/b.go (no directory separator).
	want := []string{"a.go", filepath.Join("sub", "c.go"), "b.go"}
	sort.Strings(want)
	if len(decoded.Matches) != len(want) {
		t.Fatalf("Matches = %v, want %v", decoded.Matches, want)
	}
}

func TestFileSearch_InvalidPatternRejected(t *testing.T) {
	var s FileSearch
	err := s.ValidateInput(context.Background(), json.RawMessage(`{"pattern":"["}`), tools.RunContext{})
	if err == nil {
		t.Fatal("ValidateInput with a malformed glob = nil error, want an error")
	}
}
