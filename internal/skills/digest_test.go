package skills

import (
	"bytes"
	"testing"
)

func TestBundleDigest_DeterministicRegardlessOfFieldOrder(t *testing.T) {
	b1 := SkillBundle{
		SkillID:         "triage-report",
		Description:     "Triages a weekly report.",
		TriggerHint:     "weekly report",
		DeclaredToolIDs: []string{"platform/file_read@v1", "platform/web_fetch@v1"},
		Files: []BundleFile{
			{Path: "template.md", Content: []byte("# Template")},
			{Path: "checklist.md", Content: []byte("- step one")},
		},
	}
	b2 := SkillBundle{
		SkillID:         "triage-report",
		Description:     "Triages a weekly report.",
		TriggerHint:     "weekly report",
		DeclaredToolIDs: []string{"platform/web_fetch@v1", "platform/file_read@v1"}, // reordered
		Files: []BundleFile{
			{Path: "checklist.md", Content: []byte("- step one")}, // reordered
			{Path: "template.md", Content: []byte("# Template")},
		},
	}
	d1, d2 := BundleDigest(b1), BundleDigest(b2)
	if !bytes.Equal(d1, d2) {
		t.Errorf("BundleDigest differs under field reordering: %x vs %x", d1, d2)
	}
}

func TestBundleDigest_ChangesWithContent(t *testing.T) {
	base := SkillBundle{SkillID: "s", Description: "d", Files: []BundleFile{{Path: "a.md", Content: []byte("v1")}}}
	changed := base
	changed.Files = []BundleFile{{Path: "a.md", Content: []byte("v2")}}

	if bytes.Equal(BundleDigest(base), BundleDigest(changed)) {
		t.Error("BundleDigest did not change when a file's content changed")
	}
}

func TestBundleDigest_ChangesWithScriptPresence(t *testing.T) {
	base := SkillBundle{SkillID: "s", Description: "d"}
	withScript := base
	withScript.ScriptContent = []byte("#!/bin/sh\necho hi\n")

	if bytes.Equal(BundleDigest(base), BundleDigest(withScript)) {
		t.Error("BundleDigest did not change when a script was added")
	}
}
