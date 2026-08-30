package skills

import (
	"bytes"
	"testing"
)

func TestBuildSkillSet_FiltersToAdmittedOnly(t *testing.T) {
	bundles := []SkillBundle{
		{SkillID: "triage-report", Description: "d1", DeclaredToolIDs: []string{"platform/file_read@v1"}},
		{SkillID: "send-email", Description: "d2", DeclaredToolIDs: []string{"platform/web_fetch@v1"}},
	}
	set := BuildSkillSet(bundles, []string{"triage-report"})
	if len(set.Entries) != 1 || set.Entries[0].SkillID != "triage-report" {
		t.Errorf("BuildSkillSet entries = %+v, want only triage-report", set.Entries)
	}
}

func TestBuildSkillSet_EmptyAdmittedSetProducesEmptySet(t *testing.T) {
	bundles := []SkillBundle{{SkillID: "triage-report", Description: "d1"}}
	set := BuildSkillSet(bundles, nil)
	if len(set.Entries) != 0 {
		t.Errorf("BuildSkillSet with no admitted ids = %+v, want empty", set.Entries)
	}
}

func TestBuildSkillSet_DigestStableUnderBundleOrder(t *testing.T) {
	a := SkillBundle{SkillID: "a-skill", Description: "d1"}
	b := SkillBundle{SkillID: "b-skill", Description: "d2"}
	admitted := []string{"a-skill", "b-skill"}

	d1 := BuildSkillSet([]SkillBundle{a, b}, admitted).Digest
	d2 := BuildSkillSet([]SkillBundle{b, a}, admitted).Digest
	if !bytes.Equal(d1, d2) {
		t.Errorf("SkillSet.Digest depends on bundle input order: %x vs %x", d1, d2)
	}
}

func TestBuildSkillSet_DigestChangesWithAdmittedSet(t *testing.T) {
	bundles := []SkillBundle{
		{SkillID: "a-skill", Description: "d1"},
		{SkillID: "b-skill", Description: "d2"},
	}
	d1 := BuildSkillSet(bundles, []string{"a-skill"}).Digest
	d2 := BuildSkillSet(bundles, []string{"a-skill", "b-skill"}).Digest
	if bytes.Equal(d1, d2) {
		t.Error("SkillSet.Digest did not change when the tenant's admitted set grew")
	}
}
