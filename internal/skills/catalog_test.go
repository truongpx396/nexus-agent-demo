package skills

import "testing"

func TestCatalog_ResolveAndReadFile(t *testing.T) {
	bundles := []SkillBundle{
		{
			SkillID:       "triage-report",
			Description:   "d",
			Files:         []BundleFile{{Path: "template.md", Content: []byte("# Template")}},
			ScriptContent: []byte("#!/bin/sh\n"),
		},
	}
	c := NewCatalog(bundles)

	b, ok := c.Resolve("triage-report")
	if !ok || b.SkillID != "triage-report" {
		t.Fatalf("Resolve(triage-report) = %+v, %v", b, ok)
	}

	content, err := c.ReadFile("triage-report", "template.md")
	if err != nil || string(content) != "# Template" {
		t.Errorf("ReadFile(template.md) = %q, %v", content, err)
	}

	if _, err := c.ReadFile("triage-report", scriptFileName); err == nil {
		t.Error("ReadFile(script) succeeded, want a refusal — a script is never tier-3 content")
	}

	if _, err := c.ReadFile("triage-report", "nonexistent.md"); err == nil {
		t.Error("ReadFile(nonexistent.md) succeeded, want an error")
	}

	if _, ok := c.Resolve("unknown-skill"); ok {
		t.Error("Resolve(unknown-skill) = true, want false")
	}
}
