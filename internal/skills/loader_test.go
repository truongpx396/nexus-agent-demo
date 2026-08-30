package skills

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func writeBundle(t *testing.T, root, skillID string, files map[string]string, withScript bool) {
	t.Helper()
	dir := filepath.Join(root, skillID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	manifest := `{"skill_id":"` + skillID + `","description":"a test skill","trigger_hint":"testing","declared_tool_ids":["platform/file_read@v1"],"signature":"` + base64.StdEncoding.EncodeToString([]byte("sig")) + `"}`
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if withScript {
		if err := os.WriteFile(filepath.Join(dir, scriptFileName), []byte("#!/bin/sh\necho hi\n"), 0o700); err != nil { //nolint:gosec // test-only: the bundle's script must be executable
			t.Fatalf("write script: %v", err)
		}
	}
}

func TestLoadBundles_ReadsManifestAndFiles(t *testing.T) {
	root := t.TempDir()
	writeBundle(t, root, "triage-report", map[string]string{"template.md": "# Template"}, false)

	bundles, err := LoadBundles(root)
	if err != nil {
		t.Fatalf("LoadBundles: %v", err)
	}
	if len(bundles) != 1 {
		t.Fatalf("LoadBundles returned %d bundles, want 1", len(bundles))
	}
	b := bundles[0]
	if b.SkillID != "triage-report" {
		t.Errorf("SkillID = %q, want triage-report", b.SkillID)
	}
	if len(b.DeclaredToolIDs) != 1 || b.DeclaredToolIDs[0] != "platform/file_read@v1" {
		t.Errorf("DeclaredToolIDs = %v, want [platform/file_read@v1]", b.DeclaredToolIDs)
	}
	if len(b.Files) != 1 || b.Files[0].Path != "template.md" || string(b.Files[0].Content) != "# Template" {
		t.Errorf("Files = %+v, want one template.md file", b.Files)
	}
	if b.HasScript() {
		t.Error("HasScript() = true, want false (no script written)")
	}
}

func TestLoadBundles_ScriptFileIsSeparatedFromReferenceFiles(t *testing.T) {
	root := t.TempDir()
	writeBundle(t, root, "send-email", map[string]string{"template.md": "content"}, true)

	bundles, err := LoadBundles(root)
	if err != nil {
		t.Fatalf("LoadBundles: %v", err)
	}
	b := bundles[0]
	if !b.HasScript() {
		t.Fatal("HasScript() = false, want true")
	}
	for _, f := range b.Files {
		if f.Path == scriptFileName {
			t.Error("script file leaked into Files; it must only appear as ScriptContent")
		}
	}
}

func TestLoadBundles_SkipsDirectoryWithNoManifest(t *testing.T) {
	root := t.TempDir()
	stray := filepath.Join(root, "not-a-bundle")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stray, "README.md"), []byte("just a note"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	bundles, err := LoadBundles(root)
	if err != nil {
		t.Fatalf("LoadBundles: %v", err)
	}
	if len(bundles) != 0 {
		t.Errorf("LoadBundles = %d bundles, want 0 for a directory with no skill.json", len(bundles))
	}
}

func TestLoadBundles_MissingRootIsNotAnError(t *testing.T) {
	bundles, err := LoadBundles(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("LoadBundles on a missing root: %v", err)
	}
	if bundles != nil {
		t.Errorf("LoadBundles on a missing root = %v, want nil", bundles)
	}
}
