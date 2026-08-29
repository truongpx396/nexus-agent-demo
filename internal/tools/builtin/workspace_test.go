package builtin

import (
	"path/filepath"
	"testing"
)

func TestResolveWorkspacePath(t *testing.T) {
	ws := "/tmp/workspace"
	cases := []struct {
		name    string
		rel     string
		wantErr bool
		want    string
	}{
		{"simple file", "notes.txt", false, filepath.Join(ws, "notes.txt")},
		{"nested file", "src/main.go", false, filepath.Join(ws, "src", "main.go")},
		{"traversal escape", "../../etc/passwd", false, filepath.Join(ws, "etc", "passwd")},
		{"absolute path treated as workspace-relative", "/etc/passwd", false, filepath.Join(ws, "etc", "passwd")},
		{"empty path", "", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveWorkspacePath(ws, tc.rel)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveWorkspacePath(%q) = %q, nil, want an error", tc.rel, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWorkspacePath(%q) error = %v", tc.rel, err)
			}
			if got != tc.want {
				t.Fatalf("resolveWorkspacePath(%q) = %q, want %q", tc.rel, got, tc.want)
			}
			if !filepathHasPrefix(got, ws) {
				t.Fatalf("resolveWorkspacePath(%q) = %q escapes workspace %q", tc.rel, got, ws)
			}
		})
	}
}

func filepathHasPrefix(path, prefix string) bool {
	rel, err := filepath.Rel(prefix, path)
	if err != nil {
		return false
	}
	return rel == "." || (len(rel) > 0 && rel[0] != '.')
}

func TestResolveWorkspacePath_NoWorkspaceConfigured(t *testing.T) {
	if _, err := resolveWorkspacePath("", "a.txt"); err == nil {
		t.Fatal("resolveWorkspacePath with no workspace = nil error, want an error")
	}
}
